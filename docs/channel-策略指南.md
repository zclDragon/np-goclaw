# Channel 策略指南

## 概述

每个 channel 实例都支持**私聊策略（DM Policy）**和**群组策略（Group Policy）**，用于控制消息准入规则。策略在 `internal/channels/channel.go` 中由 `BaseChannel` 统一实现，各平台 channel 在收到消息时调用对应方法。

---

## DM Policy（私聊策略）

控制是否接受私聊消息，以及如何处理陌生人。

定义在 [channel.go](file:///internal/channels/channel.go)：

```go
type DMPolicy string

const (
    DMPolicyPairing   DMPolicy = "pairing"   // 需要配对码验证
    DMPolicyAllowlist DMPolicy = "allowlist" // 仅白名单用户
    DMPolicyOpen      DMPolicy = "open"      // 接受所有人
    DMPolicyDisabled  DMPolicy = "disabled"  // 拒绝所有私聊
)
```

| 策略 | 行为 | 适用场景 |
|------|------|----------|
| `pairing`（默认） | 陌生人需配对码验证；已在 allowlist 或已配对过的用户直接放行 | 团队 bot，兼顾安全与便利 |
| `allowlist` | 只有 `allow_from` 白名单中的用户能发私聊 | 仅指定管理员使用 |
| `open` | 任何人都能发私聊，无需验证 | 公开服务 bot |
| `disabled` | 拒绝所有私聊 | 仅群聊 bot |

### CheckDMPolicy 判断流程

```
收到私聊消息
  ├─ disabled → PolicyDeny（拒绝）
  ├─ open → PolicyAllow（放行）
  ├─ allowlist → 在 allow_from 中才 PolicyAllow，否则 PolicyDeny
  └─ pairing（默认） →
       ├─ 在 allow_from 中 → PolicyAllow
       ├─ 数据库中已有配对记录 → PolicyAllow
       └─ 都没有 → PolicyNeedsPairing（需要配对）
```

---

## Group Policy（群组策略）

控制是否接受群组消息。

定义在 [channel.go](file:///internal/channels/channel.go)：

```go
type GroupPolicy string

const (
    GroupPolicyOpen      GroupPolicy = "open"      // 接受所有群组
    GroupPolicyAllowlist GroupPolicy = "allowlist" // 仅白名单用户
    GroupPolicyDisabled  GroupPolicy = "disabled"  // 拒绝所有群消息
)
```

| 策略 | 行为 | 适用场景 |
|------|------|----------|
| `open`（默认） | 所有群组消息都接受 | 公开群聊 bot |
| `allowlist` | 只有白名单用户发的群消息才处理 | 受限群组 |
| `disabled` | 不处理任何群消息 | 仅私聊 bot |
| `pairing` | 群组也需要配对验证 | 私密群组 |

### CheckGroupPolicy 判断流程

```
收到群消息
  ├─ disabled → PolicyDeny（拒绝）
  ├─ allowlist → 发送者在 allow_from 中才 PolicyAllow
  ├─ pairing →
  │    ├─ 在 allow_from 中 → PolicyAllow
  │    ├─ 群已被 approve（内存缓存）→ PolicyAllow
  │    ├─ 数据库中有配对记录 → PolicyAllow 并缓存
  │    └─ 都没有 → PolicyNeedsPairing
  └─ open → PolicyAllow（放行）
```

---

## Pairing（配对）机制

当策略设为 `pairing` 时触发，是新用户（或新群组）的准入流程。

### 配对流程

```
新用户发消息
  → 检测到未配对
  → 生成 8 位配对码（60 分钟有效）
  → 回复用户配对指引（同用户 60 秒内不重复发送）
  → 管理员执行配对批准
  → 用户再次发消息 → 数据库查到配对记录 → 放行
```

### 配对码规格

| 项目 | 值 |
|------|-----|
| 长度 | 8 字符 |
| 字符集 | `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`（排除易混淆字符：0、O、1、I、L） |
| 有效期 | 60 分钟 |
| 每用户最大待批准数 | 3 |
| 重复提醒间隔 | 60 秒 |

### 管理员批准

```
goclaw pairing approve <配对码>
```

### 群组配对

群组配对使用 `"group:" + chatID` 作为标识符存入数据库。批准后的群组会被缓存到内存中（`MarkGroupApproved`），后续消息无需再查数据库。

---

## 各 Channel 默认策略

| Channel | DM 默认策略 | Group 默认策略 |
|---------|-----------|--------------|
| Telegram | `pairing` | `open` |
| Feishu/Lark | `pairing` | `open` |
| Discord | `pairing` | `pairing` |
| Slack | `pairing` | `pairing` |
| WhatsApp | `pairing` | `pairing` |
| Zalo OA | `pairing` | N/A（仅 DM） |
| Zalo Personal | `allowlist` | `allowlist` |
| **WeCom** | **`pairing`** | **`pairing`** |

---

## 配置方式

### Web UI（推荐）

创建 channel 实例时，在表单中直接选择策略：

- **DM Policy**：`pairing` / `open` / `allowlist` / `disabled`
- **Group Policy**：`open` / `allowlist` / `disabled` / `pairing`

### 静态配置文件

在 `channels` 配置中指定：

```yaml
channels:
  wecom:
    enabled: true
    bot_id: "xxx"
    bot_secret: "xxx"
    dm_policy: "pairing"
    group_policy: "pairing"
    allow_from:
      - "admin_user_id"
```

---

## 场景推荐

| 场景 | DM Policy | Group Policy |
|------|-----------|--------------|
| 个人使用（仅自己） | `open` | `disabled` |
| 团队 bot（安全优先） | `pairing` | `pairing` |
| 仅指定管理员使用 | `allowlist` | `allowlist` |
| 公开服务 bot | `open` | `open` |

> **注意**：`allowlist` 策略需要同时配置 `allow_from` 字段，否则所有消息都会被拒绝。系统会在启动时对空白名单 + allowlist 策略的组合输出 warning 日志。