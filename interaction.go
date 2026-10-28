package beaktelegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	platform "github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/internal/telegram"
	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/sdk"
)

func sendTelegramInteraction(ctx context.Context, runtime sdk.Runtime, req sdk.OutboundMessage) (*sdk.SendResult, error) {
	account, err := selectRuntimeAccount(runtime, req.AccountUUID)
	if err != nil {
		return nil, err
	}
	chatID, business, direct, err := telegramOutboundChatTarget(req.ChatID, req.Raw)
	if err != nil {
		return nil, err
	}
	thread, threadDirect, err := telegramOutboundThreadTarget(req.ThreadID, req.Raw)
	if err != nil {
		return nil, err
	}
	if direct == "" {
		direct = threadDirect
	}
	client := platform.NewClient("", credentialStrings(account.Credential))
	client.HTTPClient = runtime.HTTPClient
	id, err := client.SendInteraction(ctx, platform.TextRoute{ChatID: chatID, BusinessConnectionID: business, DirectMessagesTopicID: direct, MessageThreadID: thread}, *req.Interaction)
	if err != nil {
		return nil, err
	}
	return &sdk.SendResult{Platform: Platform, AccountUUID: accountKey(account), MessageID: id}, nil
}

func handleTelegramInteraction(ctx context.Context, runtime sdk.Runtime, account sdk.ChannelAccount, body []byte) (bool, error) {
	var update struct {
		Callback *struct {
			ID   string `json:"id"`
			Data string `json:"data"`
			From struct {
				ID    int64 `json:"id"`
				IsBot bool  `json:"is_bot"`
			} `json:"from"`
			Message *platform.Message `json:"message"`
		} `json:"callback_query"`
	}
	if err := json.Unmarshal(body, &update); err != nil {
		return false, webhookError(http.StatusBadRequest, "invalid_request_body", err)
	}
	callback := update.Callback
	if callback == nil {
		return false, nil
	}
	if callback.From.IsBot || callback.Message == nil || callback.Message.MessageID == 0 || callback.From.ID == 0 {
		return true, fmt.Errorf("telegram callback identity missing")
	}
	action, err := sdk.ParseInteractionToken(callback.Data)
	if err != nil {
		return true, err
	}
	action.ActorID = strconv.FormatInt(callback.From.ID, 10)
	action.ChatID = telegramInboundChatID(callback.Message.BusinessConnectionID,
		strconv.FormatInt(callback.Message.Chat.ID, 10), telegramDirectMessagesTopicID(callback.Message))
	action.MessageID = strconv.FormatInt(callback.Message.MessageID, 10)
	action.EventID = callback.ID
	if _, err := sdk.DispatchInteraction(ctx, runtime, action); err != nil {
		return true, err
	}
	client := platform.NewClient("", credentialStrings(account.Credential))
	client.HTTPClient = runtime.HTTPClient
	return true, client.AnswerInteraction(ctx, callback.ID)
}
