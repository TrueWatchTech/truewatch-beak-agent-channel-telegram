# Telegram Channel SDK

`github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram` 将 Telegram
机器人账号接入 Beak Channel Gateway。它实现统一 `sdk.Connector`，并把
Telegram 平台差异全部封装在 SDK 内部。

平台逻辑包括凭证校验、webhook 验签、Update 标准化、出站发送、topic
thread、引用消息和表情确认。使用 `NewConnector()` 获取可直接接入 Beak 的
connector。

## Module

```
github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram
```

## 使用示例

```go
import (
    "fmt"

    beak "github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram"
)

func main() {
    connector := beak.NewConnector()
    fmt.Println(connector.Metadata().Label)
}
```

## 凭证字段

以下字段会出现在 Beak 控制台表单（`CredentialSchema`）中：

| Key | 名称 | Secret | 必填 |
| --- | --- | --- | --- |
| `bot_token` | Bot Token | 是 | 是 |

`webhook_secret` 不再由用户或前端生成。`ValidateCredential` 在 bot token
校验成功后使用加密安全随机数生成该值，并通过返回的 credential 交给 Beak
加密持久化。已有账号中合法的 secret 会继续沿用，不会在重启时轮换。

后端字段（base URL、回调地址、webhook secret、offset、token 缓存）不会出现在表单中。

## 入站投递

模式：**webhook**。当前 SDK 不启动 `getUpdates` 轮询。

由 Beak host 持有 HTTP endpoint，并通过 `sdk.Runtime.Webhook.URL` 把当前账号的
绝对 HTTPS 地址显式传给 SDK。`Start` 使用已持久化的 `bot_token` 和
`webhook_secret` 调用 Telegram `setWebhook`；每次 runtime 启动都会幂等注册，
因此服务重启或 runtime 迁移后不需要人工操作。Telegram 随后通过
`X-Telegram-Bot-Api-Secret-Token` 请求头回传 secret，SDK 会在解析 Update 前
校验。host 未提供绝对 HTTPS URL 或账号缺失 secret 时，`Start` 会明确失败，
不会把未注册 webhook 的账号标记为正常运行。启用 webhook 后，不要再为同一个
bot 启用 `getUpdates`。

支持的入站类型：

- `message`、`channel_post`、`business_message`
- 文本和媒体 caption
- forum topic 的 `message_thread_id`
- 通过 `is_direct_messages` 和 `direct_messages_topic` 接入私聊 topic
- 通过 `reply_to_message` 输出统一 `ReferencedMessage`

Telegram 还会投递 `edited_message`、`edited_channel_post` 和
`edited_business_message`。在 Beak 通用合同提供消息更新操作之前，SDK 会识别并
明确忽略这些事件；如果把编辑事件当成一条新消息，会错误地再次触发 Agent。

Business update 的 `business_connection_id` 会编码进对 Beak 透明的通用
`ChatID`，避免它和数字 chat id 相同的普通 bot 会话串线。`Send` 会还原目标并把
connection id 传给 `sendMessage`；`sender_business_bot` 标记的自身出站消息会按
self echo 过滤。

私聊 topic 使用对 host 透明的通用 `ChatID` 和 `ThreadID` 表达。`Send` 会自行
还原这些值并填写 `direct_messages_topic_id`，Beak adapter 不解析 Telegram 字段。

Telegram entity 的 offset/length 按 UTF-16 code unit 解析。SDK 会安全移除
bot mention；只有 `@bot`、正文为空的消息仍会以 `MentionedMe=true` 交给
Beak 处理 follow-up。

## 出站与确认

`Send` 把统一文本、thread 和 username mention 映射到 `sendMessage`。
超过 Telegram 4096 字符限制的文本会自动安全分片。通用 CommonMark 当前
降级为 text，因为 CommonMark 不等于 Telegram HTML/MarkdownV2；SDK 不会再把
未经转换的 Markdown 标记成 HTML。

分片发送必须携带稳定的通用 `MessageUUID`，并提供
`sdk.Runtime.AccountStore`。SDK 会把每个已成功分片记录到
`outbound_deliveries`，普通重试从第一个未发送分片继续，不会重新发送前面的分片。

Telegram 没有可移植的 `@all`，所以 `MentionAll` 会返回明确错误；username
mention 可用。`Acknowledge` 通过 `setMessageReaction` 支持统一 `reaction`
模式，并将 `processing`、`thinking`、`done` 等 intent 映射为平台表情。

## 身份与去重

`ValidateCredential` 调用 `getMe`。稳定的数字 bot id 用作 `AccountKey` 和
`bot_identity.id`；username 只用于展示和 mention 匹配。入站优先使用
`update_id` 去重；fallback 同时包含 `chat_id + message_id`，因为
`message_id` 在不同 chat 间并不全局唯一。
明确的鉴权失败返回 `Valid=false`；临时网络、限流和平台服务错误会短暂重试，
仍失败时返回 Go error，避免 host 把有效 token 错误标记为无效。

## State

账号维度状态位于 `state/state.go`，通用集合和标准 `stream_*` runtime health
字段通过 `sdk.AccountStore` 持久化。成功收到 webhook 时更新 activity；只有
消息真正写入 Beak 后才更新 `stream_last_event_at`。SDK 保存状态时会保留 host
写入的未知字段。尚未观测到的 health 时间字段会省略，不会写成公元 1 年时间。

## 验证

```bash
go test ./...
cd conformance && go test ./...
```

工作区级 `beak-channel-sdk-conformance-tests` 也包含真实 Telegram adapter，
覆盖凭证身份、UTF-16 mention、纯 mention、thread/引用、caption、reaction 和
结构化 webhook 错误。
