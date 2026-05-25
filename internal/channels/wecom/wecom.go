package wecom

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	aibot "github.com/go-sphere/wecom-aibot-go-sdk/aibot"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	channelmedia "github.com/nextlevelbuilder/goclaw/internal/channels/media"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	defaultWorkingMessage = "Working on it..."
	pairingDebounce       = 60 * time.Second
)

type Channel struct {
	*channels.BaseChannel
	client         *aibot.WSClient
	botID          string
	botSecret      string
	dmPolicy       string
	groupPolicy    string
	dmStream       bool
	groupStream    bool
	workingMessage string
	blockReply     *bool
	wsURL          string
	frames         sync.Map
	activeStreams  sync.Map
}

func New(cfg config.WeComConfig, msgBus *bus.MessageBus, pairingSvc store.PairingStore) (*Channel, error) {
	if cfg.BotID == "" {
		return nil, fmt.Errorf("wecom bot_id is required")
	}
	if cfg.BotSecret == "" {
		return nil, fmt.Errorf("wecom bot_secret is required")
	}

	base := channels.NewBaseChannel(channels.TypeWeCom, msgBus, cfg.AllowFrom)
	base.ValidatePolicy(cfg.DMPolicy, cfg.GroupPolicy)

	dmPolicy := cfg.DMPolicy
	if dmPolicy == "" {
		dmPolicy = "pairing"
	}
	groupPolicy := cfg.GroupPolicy
	if groupPolicy == "" {
		groupPolicy = "pairing"
	}

	dmStream := true
	if cfg.DMStream != nil {
		dmStream = *cfg.DMStream
	}
	groupStream := false
	if cfg.GroupStream != nil {
		groupStream = *cfg.GroupStream
	}

	workingMessage := cfg.WorkingMessage
	if strings.TrimSpace(workingMessage) == "" {
		workingMessage = defaultWorkingMessage
	}

	ch := &Channel{
		BaseChannel:    base,
		botID:          cfg.BotID,
		botSecret:      cfg.BotSecret,
		dmPolicy:       dmPolicy,
		groupPolicy:    groupPolicy,
		dmStream:       dmStream,
		groupStream:    groupStream,
		workingMessage: workingMessage,
		blockReply:     cfg.BlockReply,
		wsURL:          cfg.WSURL,
	}
	ch.SetPairingService(pairingSvc)
	return ch, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if c.IsRunning() {
		return nil
	}

	opts := aibot.WSClientOptions{
		BotID:  c.botID,
		Secret: c.botSecret,
		Logger: wecomLogger{},
	}
	if c.wsURL != "" {
		opts.WSURL = c.wsURL
	}

	client := aibot.NewWSClient(opts)
	client.OnAuthenticated(func() {
		c.MarkHealthy("Connected")
		slog.Info("wecom channel authenticated", "channel", c.Name())
	})
	client.OnDisconnected(func(reason string) {
		c.MarkDegraded("Disconnected", reason, channels.ChannelFailureKindNetwork, true)
	})
	client.OnError(func(err error) {
		if err != nil {
			c.MarkDegraded("Runtime warning", err.Error(), channels.ChannelFailureKindUnknown, true)
			slog.Warn("wecom channel error", "channel", c.Name(), "error", err)
		}
	})
	client.OnMessageText(c.handleText)
	client.OnMessageMixed(c.handleMixed)
	client.OnMessageVoice(c.handleVoice)
	client.OnMessageImage(c.handleImage)
	client.OnMessageFile(c.handleFile)
	client.OnMessageVideo(c.handleVideo)

	c.client = client
	client.Connect()
	c.SetRunning(true)

	go func() {
		<-ctx.Done()
		_ = c.Stop(context.Background())
	}()

	return nil
}

func (c *Channel) Stop(context.Context) error {
	if c.client != nil {
		c.client.Disconnect()
	}
	c.SetRunning(false)
	c.frames.Range(func(key, value any) bool {
		c.frames.Delete(key)
		return true
	})
	c.activeStreams.Range(func(key, value any) bool {
		c.activeStreams.Delete(key)
		return true
	})
	return nil
}

func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if c.client == nil {
		return fmt.Errorf("wecom client is not available")
	}
	content := strings.TrimSpace(msg.Content)
	localKey := ""
	if msg.Metadata != nil {
		localKey = msg.Metadata["local_key"]
	}
	if localKey != "" {
		if v, ok := c.activeStreams.Load(localKey); ok {
			stream := v.(*wecomStream)
			defer c.cleanupStream(localKey)
			if content == "" {
				content = stream.lastText()
			}
			if err := stream.finish(ctx, content); err != nil {
				return err
			}
			return c.sendOutboundMedia(ctx, msg.ChatID, localKey, msg.Media)
		}
	}
	if content != "" {
		if _, err := c.client.SendMarkdown(msg.ChatID, content); err != nil {
			return err
		}
	}
	return c.sendOutboundMedia(ctx, msg.ChatID, localKey, msg.Media)
}

func (c *Channel) sendOutboundMedia(ctx context.Context, chatID, replyKey string, attachments []bus.MediaAttachment) error {
	for _, attachment := range attachments {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if attachment.URL == "" {
			continue
		}
		mediaType := wecomMediaType(attachment.URL, attachment.ContentType)
		data, err := os.ReadFile(attachment.URL)
		if err != nil {
			return fmt.Errorf("read wecom media %s: %w", attachment.URL, err)
		}
		mediaID, err := c.uploadMedia(data, mediaType, filepath.Base(attachment.URL))
		if err != nil {
			return fmt.Errorf("upload wecom media %s: %w", attachment.URL, err)
		}
		videoOptions := wecomVideoOptions(mediaType, mediaID, attachment)
		if frame, ok := c.lookupFrame(replyKey); ok {
			if _, err := c.client.ReplyMedia(frame, mediaType, mediaID, videoOptions); err != nil {
				return fmt.Errorf("reply wecom media %s: %w", attachment.URL, err)
			}
			continue
		}
		if _, err := c.client.SendMediaMessage(chatID, mediaType, mediaID, videoOptions); err != nil {
			return fmt.Errorf("send wecom media %s: %w", attachment.URL, err)
		}
	}
	return nil
}

type uploadMediaFinishResult struct {
	Type    aibot.WeComMediaType `json:"type"`
	MediaID string               `json:"media_id"`
}

func (c *Channel) uploadMedia(data []byte, mediaType aibot.WeComMediaType, filename string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("wecom client is not available")
	}

	totalSize := len(data)
	const chunkSize = 512 * 1024
	totalChunks := (totalSize + chunkSize - 1) / chunkSize
	if totalChunks > 100 {
		return "", fmt.Errorf("file too large: %d chunks exceeds maximum of 100", totalChunks)
	}

	md5Hash := md5.Sum(data)
	md5Str := hex.EncodeToString(md5Hash[:])

	initFrame := &aibot.WsFrame{Headers: aibot.WsFrameHeaders{ReqID: aibot.GenerateReqId("upload_init")}}
	initResult, err := c.client.Reply(initFrame, map[string]interface{}{
		"type":         mediaType,
		"filename":     filename,
		"total_size":   totalSize,
		"total_chunks": totalChunks,
		"md5":          md5Str,
	}, aibot.WsCmd.UPLOAD_MEDIA_INIT)
	if err != nil {
		return "", fmt.Errorf("upload init failed: %w", err)
	}
	var initResp struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(initResult.Body, &initResp); err != nil {
		return "", fmt.Errorf("parse init response: %w", err)
	}
	if initResp.UploadID == "" {
		return "", fmt.Errorf("upload init returned empty upload_id")
	}

	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > totalSize {
			end = totalSize
		}
		b64 := base64.StdEncoding.EncodeToString(data[start:end])
		chunkFrame := &aibot.WsFrame{Headers: aibot.WsFrameHeaders{ReqID: aibot.GenerateReqId("upload_chunk")}}
		if _, err := c.client.Reply(chunkFrame, map[string]interface{}{
			"upload_id":   initResp.UploadID,
			"chunk_index": i,
			"base64_data": b64,
		}, aibot.WsCmd.UPLOAD_MEDIA_CHUNK); err != nil {
			return "", fmt.Errorf("chunk %d upload failed: %w", i, err)
		}
	}

	finishFrame := &aibot.WsFrame{Headers: aibot.WsFrameHeaders{ReqID: aibot.GenerateReqId("upload_finish")}}
	finishResult, err := c.client.Reply(finishFrame, map[string]interface{}{
		"upload_id": initResp.UploadID,
	}, aibot.WsCmd.UPLOAD_MEDIA_FINISH)
	if err != nil {
		return "", fmt.Errorf("upload finish failed: %w", err)
	}

	var finishResp uploadMediaFinishResult
	if err := json.Unmarshal(finishResult.Body, &finishResp); err != nil {
		return "", fmt.Errorf("parse finish response: %w", err)
	}
	if finishResp.MediaID == "" {
		return "", fmt.Errorf("upload finish returned empty media_id")
	}

	return finishResp.MediaID, nil
}

func wecomMediaType(path, contentType string) aibot.WeComMediaType {
	ct := strings.ToLower(contentType)
	if ct == "" {
		ct = channelmedia.DetectMIMEType(path)
	}
	switch {
	case strings.HasPrefix(ct, "image/"):
		return aibot.WeComMediaTypeImage
	case strings.HasPrefix(ct, "video/"):
		return aibot.WeComMediaTypeVideo
	case strings.HasPrefix(ct, "audio/"):
		return aibot.WeComMediaTypeVoice
	default:
		return aibot.WeComMediaTypeFile
	}
}

func wecomVideoOptions(mediaType aibot.WeComMediaType, mediaID string, attachment bus.MediaAttachment) *aibot.VideoMediaContent {
	if mediaType != aibot.WeComMediaTypeVideo {
		return nil
	}
	title := attachment.Caption
	if title == "" {
		title = filepath.Base(attachment.URL)
	}
	return &aibot.VideoMediaContent{MediaID: mediaID, Title: title}
}

func (c *Channel) IsRunning() bool {
	return c.BaseChannel.IsRunning()
}

func (c *Channel) StreamEnabled(isGroup bool) bool {
	if isGroup {
		return c.groupStream
	}
	return c.dmStream
}

func (c *Channel) CreateStream(ctx context.Context, chatID string, firstStream bool) (channels.ChannelStream, error) {
	if c.client == nil {
		return nil, fmt.Errorf("wecom client is not available")
	}
	frame, ok := c.lookupFrame(chatID)
	if !ok {
		return nil, fmt.Errorf("wecom stream frame not found for %s", chatID)
	}
	streamID := aibot.GenerateReqId("stream")
	stream := &wecomStream{channel: c, key: chatID, frame: frame, streamID: streamID}
	c.activeStreams.Store(chatID, stream)
	if c.workingMessage != "" {
		stream.Update(ctx, c.workingMessage)
	}
	return stream, nil
}

func (c *Channel) FinalizeStream(context.Context, string, channels.ChannelStream) {}

func (c *Channel) ReasoningStreamEnabled() bool { return false }

func (c *Channel) BlockReplyEnabled() *bool { return c.blockReply }

func (c *Channel) handleText(frame *aibot.WsFrame) {
	var msg aibot.TextMessage
	if err := aibot.ParseMessageBody(frame, &msg); err != nil {
		slog.Warn("wecom: parse text message failed", "error", err)
		return
	}
	content := strings.TrimSpace(msg.Text.Content)
	if msg.Quote != nil && msg.Quote.Text != nil && strings.TrimSpace(msg.Quote.Text.Content) != "" {
		content += "\nQuote message: " + strings.TrimSpace(msg.Quote.Text.Content)
	}
	if content == "" {
		return
	}
	c.publishMessage(context.Background(), frame, msg.BaseMessage, content, nil)
}

func (c *Channel) handleVoice(frame *aibot.WsFrame) {
	var msg aibot.VoiceMessage
	if err := aibot.ParseMessageBody(frame, &msg); err != nil {
		slog.Warn("wecom: parse voice message failed", "error", err)
		return
	}
	content := strings.TrimSpace(msg.Voice.Content)
	if content == "" {
		content = "[voice]"
	}
	c.publishMessage(context.Background(), frame, msg.BaseMessage, content, nil)
}

func (c *Channel) handleMixed(frame *aibot.WsFrame) {
	var msg aibot.MixedMessage
	if err := aibot.ParseMessageBody(frame, &msg); err != nil {
		slog.Warn("wecom: parse mixed message failed", "error", err)
		return
	}
	var parts []string
	var mediaPaths []string
	var mediaInfos []channelmedia.MediaInfo
	for _, item := range msg.Mixed.MsgItem {
		switch item.MsgType {
		case string(aibot.MessageTypeText):
			if item.Text != nil && strings.TrimSpace(item.Text.Content) != "" {
				parts = append(parts, strings.TrimSpace(item.Text.Content))
			}
		case string(aibot.MessageTypeImage):
			if item.Image != nil {
				path, info, err := c.downloadInboundMedia(item.Image.URL, item.Image.AesKey, channelmedia.TypeImage)
				if err != nil {
					slog.Warn("wecom: download mixed image failed", "message_id", msg.MsgID, "error", err)
					parts = append(parts, "[image — download failed]")
					continue
				}
				mediaPaths = append(mediaPaths, path)
				mediaInfos = append(mediaInfos, info)
			}
		}
	}
	if tags := channelmedia.BuildMediaTags(mediaInfos); tags != "" {
		parts = append(parts, tags)
	}
	content := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if content == "" {
		content = "[mixed message]"
	}
	c.publishMessage(context.Background(), frame, msg.BaseMessage, content, mediaPaths)
}

func (c *Channel) handleImage(frame *aibot.WsFrame) {
	var msg aibot.ImageMessage
	if err := aibot.ParseMessageBody(frame, &msg); err != nil {
		slog.Warn("wecom: parse image message failed", "error", err)
		return
	}
	path, info, err := c.downloadInboundMedia(msg.Image.URL, msg.Image.AesKey, channelmedia.TypeImage)
	if err != nil {
		slog.Warn("wecom: download image failed", "message_id", msg.MsgID, "error", err)
		c.publishMessage(context.Background(), frame, msg.BaseMessage, "[image — download failed]", nil)
		return
	}
	c.publishMessage(context.Background(), frame, msg.BaseMessage, channelmedia.BuildMediaTags([]channelmedia.MediaInfo{info}), []string{path})
}

func (c *Channel) handleFile(frame *aibot.WsFrame) {
	var msg aibot.FileMessage
	if err := aibot.ParseMessageBody(frame, &msg); err != nil {
		slog.Warn("wecom: parse file message failed", "error", err)
		return
	}
	path, info, err := c.downloadInboundMedia(msg.File.URL, msg.File.AesKey, channelmedia.TypeDocument)
	if err != nil {
		slog.Warn("wecom: download file failed", "message_id", msg.MsgID, "error", err)
		c.publishMessage(context.Background(), frame, msg.BaseMessage, "[file — download failed]", nil)
		return
	}
	c.publishMessage(context.Background(), frame, msg.BaseMessage, channelmedia.BuildMediaTags([]channelmedia.MediaInfo{info}), []string{path})
}

func (c *Channel) handleVideo(frame *aibot.WsFrame) {
	var msg aibot.VideoMessage
	if err := aibot.ParseMessageBody(frame, &msg); err != nil {
		slog.Warn("wecom: parse video message failed", "error", err)
		return
	}
	path, info, err := c.downloadInboundMedia(msg.Video.URL, msg.Video.AesKey, channelmedia.TypeVideo)
	if err != nil {
		slog.Warn("wecom: download video failed", "message_id", msg.MsgID, "error", err)
		c.publishMessage(context.Background(), frame, msg.BaseMessage, "[video — download failed]", nil)
		return
	}
	c.publishMessage(context.Background(), frame, msg.BaseMessage, channelmedia.BuildMediaTags([]channelmedia.MediaInfo{info}), []string{path})
}

func (c *Channel) downloadInboundMedia(fileURL, aesKey, fallbackType string) (string, channelmedia.MediaInfo, error) {
	var empty channelmedia.MediaInfo
	if c.client == nil {
		return "", empty, fmt.Errorf("wecom client is not available")
	}
	if fileURL == "" {
		return "", empty, fmt.Errorf("media url is empty")
	}
	data, fileName, err := c.client.DownloadFile(fileURL, aesKey)
	if err != nil {
		return "", empty, err
	}
	if fileName == "" {
		fileName = fallbackType + "_" + fmt.Sprint(time.Now().UnixMilli())
	}
	ext := filepath.Ext(fileName)
	contentType := channelmedia.DetectMIMEType(fileName)
	if ext == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
		ext = extFromContentType(contentType, fallbackType)
		if filepath.Ext(fileName) == "" {
			fileName += ext
		}
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("wecom_%s_%d%s", fallbackType, time.Now().UnixMilli(), ext))
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", empty, err
	}
	mediaType := channelmedia.MediaKindFromMime(contentType)
	if fallbackType == channelmedia.TypeImage {
		mediaType = channelmedia.TypeImage
	} else if fallbackType == channelmedia.TypeVideo {
		mediaType = channelmedia.TypeVideo
	}
	info := channelmedia.MediaInfo{
		Type:        mediaType,
		FilePath:    path,
		SourceURL:   fileURL,
		ContentType: contentType,
		FileName:    fileName,
		FileSize:    int64(len(data)),
	}
	return path, info, nil
}

func extFromContentType(contentType, fallbackType string) string {
	switch {
	case strings.HasPrefix(contentType, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(contentType, "image/png"):
		return ".png"
	case strings.HasPrefix(contentType, "image/gif"):
		return ".gif"
	case strings.HasPrefix(contentType, "image/webp"):
		return ".webp"
	case strings.HasPrefix(contentType, "video/mp4"):
		return ".mp4"
	case strings.HasPrefix(contentType, "audio/ogg"):
		return ".ogg"
	case strings.HasPrefix(contentType, "audio/mpeg"):
		return ".mp3"
	case fallbackType == channelmedia.TypeImage:
		return ".jpg"
	case fallbackType == channelmedia.TypeVideo:
		return ".mp4"
	default:
		return ".bin"
	}
}

func (c *Channel) publishMessage(ctx context.Context, frame *aibot.WsFrame, msg aibot.BaseMessage, content string, media []string) {
	ctx = store.WithTenantID(ctx, c.TenantID())
	senderID := msg.From.UserID
	if senderID == "" {
		slog.Warn("wecom: dropping message with empty sender id", "message_id", msg.MsgID)
		return
	}
	chatID := msg.ChatID
	if chatID == "" {
		chatID = senderID
	}
	peerKind := "direct"
	if msg.ChatType == "group" {
		peerKind = "group"
	}
	if !c.checkPolicy(ctx, peerKind, senderID, chatID) {
		return
	}
	localKey := wecomLocalKey(msg.MsgID)
	metadata := map[string]string{
		"message_id":       msg.MsgID,
		"local_key":        localKey,
		"platform":         "wecom",
		"wecom_chat_type":  msg.ChatType,
		"wecom_aibot_id":   msg.AibotID,
		"wecom_message_id": msg.MsgID,
	}
	c.storeFrame(localKey, msg.MsgID, chatID, frame)
	c.HandleMessage(senderID, chatID, content, media, metadata, peerKind)
}

func (c *Channel) checkPolicy(ctx context.Context, peerKind, senderID, chatID string) bool {
	if peerKind == "group" {
		result := c.CheckGroupPolicy(ctx, senderID, chatID, c.groupPolicy)
		switch result {
		case channels.PolicyAllow:
			return true
		case channels.PolicyNeedsPairing:
			c.sendPairingReply(ctx, "group:"+chatID, chatID)
			return false
		default:
			slog.Debug("wecom group message rejected by policy", "sender_id", senderID, "chat_id", chatID, "policy", c.groupPolicy)
			return false
		}
	}
	result := c.CheckDMPolicy(ctx, senderID, c.dmPolicy)
	switch result {
	case channels.PolicyAllow:
		return true
	case channels.PolicyNeedsPairing:
		c.sendPairingReply(ctx, senderID, chatID)
		return false
	default:
		slog.Debug("wecom dm message rejected by policy", "sender_id", senderID, "policy", c.dmPolicy)
		return false
	}
}

func (c *Channel) sendPairingReply(ctx context.Context, senderID, chatID string) {
	ps := c.PairingService()
	if ps == nil || c.client == nil {
		return
	}
	if !c.CanSendPairingNotif(senderID, pairingDebounce) {
		return
	}
	code, err := ps.RequestPairing(ctx, senderID, c.Name(), chatID, "default", nil)
	if err != nil {
		slog.Debug("wecom pairing request failed", "sender_id", senderID, "error", err)
		return
	}
	replyText := fmt.Sprintf("GoClaw: access not configured.\n\nYour WeCom user id: %s\n\nPairing code: %s\n\nAsk the bot owner to approve with:\n  goclaw pairing approve %s", senderID, code, code)
	if _, err := c.client.SendMarkdown(chatID, replyText); err != nil {
		slog.Warn("failed to send wecom pairing reply", "error", err)
		return
	}
	c.MarkPairingNotifSent(senderID)
}

func (c *Channel) storeFrame(localKey, msgID, chatID string, frame *aibot.WsFrame) {
	if frame == nil {
		return
	}
	if localKey != "" {
		c.frames.Store(localKey, frame)
	}
	if msgID != "" {
		c.frames.Store(msgID, frame)
	}
	if chatID != "" {
		c.frames.Store(chatID, frame)
	}
}

func (c *Channel) lookupFrame(key string) (*aibot.WsFrame, bool) {
	if v, ok := c.frames.Load(key); ok {
		if frame, ok := v.(*aibot.WsFrame); ok {
			return frame, true
		}
	}
	return nil, false
}

func (c *Channel) cleanupStream(key string) {
	c.activeStreams.Delete(key)
	if frame, ok := c.lookupFrame(key); ok {
		msgID := aibot.GetMsgID(frame)
		if msgID != "" {
			c.frames.Delete(msgID)
		}
	}
	c.frames.Delete(key)
}

func wecomLocalKey(msgID string) string {
	if msgID == "" {
		return ""
	}
	return "wecom_msg_" + msgID
}

type wecomStream struct {
	channel  *Channel
	key      string
	frame    *aibot.WsFrame
	streamID string
	mu       sync.Mutex
	text     string
}

func (s *wecomStream) Update(ctx context.Context, text string) {
	_ = ctx
	s.mu.Lock()
	s.text = text
	s.mu.Unlock()
	_, err := s.channel.client.ReplyStream(s.frame, s.streamID, text, false, nil, nil)
	if err != nil {
		slog.Debug("wecom stream update failed", "error", err)
	}
}

func (s *wecomStream) Stop(context.Context) error { return nil }

func (s *wecomStream) MessageID() int { return 0 }

func (s *wecomStream) finish(ctx context.Context, text string) error {
	_ = ctx
	s.mu.Lock()
	if text == "" {
		text = s.text
	}
	s.text = text
	s.mu.Unlock()
	_, err := s.channel.client.ReplyStream(s.frame, s.streamID, text, true, nil, nil)
	return err
}

func (s *wecomStream) lastText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text
}

type wecomLogger struct{}

func (wecomLogger) Debug(format string, v ...interface{}) { slog.Debug(fmt.Sprintf(format, v...)) }
func (wecomLogger) Info(format string, v ...interface{})  { slog.Info(fmt.Sprintf(format, v...)) }
func (wecomLogger) Warn(format string, v ...interface{})  { slog.Warn(fmt.Sprintf(format, v...)) }
func (wecomLogger) Error(format string, v ...interface{}) { slog.Error(fmt.Sprintf(format, v...)) }

var _ channels.Channel = (*Channel)(nil)
var _ channels.StreamingChannel = (*Channel)(nil)
var _ channels.BlockReplyChannel = (*Channel)(nil)
