package wecom

import (
	"encoding/json"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

type wecomCreds struct {
	BotID     string `json:"bot_id"`
	BotSecret string `json:"bot_secret"`
}

type wecomInstanceConfig struct {
	DMPolicy       string   `json:"dm_policy,omitempty"`
	GroupPolicy    string   `json:"group_policy,omitempty"`
	AllowFrom      []string `json:"allow_from,omitempty"`
	DMStream       *bool    `json:"dm_stream,omitempty"`
	GroupStream    *bool    `json:"group_stream,omitempty"`
	WorkingMessage string   `json:"working_message,omitempty"`
	WSURL          string   `json:"ws_url,omitempty"`
	BlockReply     *bool    `json:"block_reply,omitempty"`
}

func Factory(name string, creds json.RawMessage, cfg json.RawMessage, msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {
	var c wecomCreds
	if len(creds) > 0 {
		if err := json.Unmarshal(creds, &c); err != nil {
			return nil, fmt.Errorf("parse wecom credentials: %w", err)
		}
	}
	if c.BotID == "" || c.BotSecret == "" {
		return nil, fmt.Errorf("wecom credentials require bot_id and bot_secret")
	}

	var ic wecomInstanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("parse wecom config: %w", err)
		}
	}

	ch, err := New(config.WeComConfig{
		Enabled:        true,
		BotID:          c.BotID,
		BotSecret:      c.BotSecret,
		AllowFrom:      config.FlexibleStringSlice(ic.AllowFrom),
		DMPolicy:       ic.DMPolicy,
		GroupPolicy:    ic.GroupPolicy,
		DMStream:       ic.DMStream,
		GroupStream:    ic.GroupStream,
		WorkingMessage: ic.WorkingMessage,
		WSURL:          ic.WSURL,
		BlockReply:     ic.BlockReply,
	}, msgBus, pairingSvc)
	if err != nil {
		return nil, err
	}
	ch.SetName(name)
	return ch, nil
}
