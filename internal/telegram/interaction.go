package telegram

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf16"

	"github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram/sdk"
)

func (c *Client) SendInteraction(ctx context.Context, route TextRoute, card sdk.InteractionCard) (string, error) {
	if err := card.Validate(); err != nil {
		return "", err
	}
	if len(card.Inputs) > 0 {
		return "", fmt.Errorf("telegram native input forms are unavailable")
	}
	text := card.Title + "\n" + card.Summary
	if len(utf16.Encode([]rune(text))) > 4096 {
		if card.WebURL == "" {
			return "", fmt.Errorf("long Telegram interaction requires an authenticated Web entry")
		}
		// Do not approve a plan from a truncated preview. The protected Web
		// page presents the complete content and owns the resulting decision.
		text = string([]rune(text)[:1800]) + "\n…内容过长，请打开网页查看并处理。"
		card.Actions = nil
	}
	rows := make([][]map[string]string, 0, len(card.Actions))
	for _, button := range card.Actions {
		token, err := card.ButtonToken(button)
		if err != nil {
			return "", err
		}
		rows = append(rows, []map[string]string{{"text": button.Label, "callback_data": token}})
	}
	if card.WebURL != "" {
		rows = append(rows, []map[string]string{{"text": "Open", "url": card.WebURL}})
	}
	payload := map[string]any{"chat_id": route.ChatID, "text": text, "reply_markup": map[string]any{"inline_keyboard": rows}}
	method := "sendMessage"
	if card.MessageID != "" {
		method = "editMessageText"
		payload["message_id"] = card.MessageID
	} else if route.MessageThreadID != "" {
		payload["message_thread_id"] = route.MessageThreadID
	}
	if route.BusinessConnectionID != "" {
		payload["business_connection_id"] = route.BusinessConnectionID
	}
	if route.DirectMessagesTopicID != "" {
		payload["direct_messages_topic_id"] = route.DirectMessagesTopicID
	}
	var result struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := c.doJSONBot(ctx, http.MethodPost, method, nil, payload, &result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", c.apiError(method, result.ErrorCode, result.Description)
	}
	if result.Result.MessageID == 0 {
		return "", fmt.Errorf("telegram interaction response has no message id")
	}
	return strconv.FormatInt(result.Result.MessageID, 10), nil
}

func (c *Client) AnswerInteraction(ctx context.Context, callbackID string) error {
	var result struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := c.doJSONBot(ctx, http.MethodPost, "answerCallbackQuery", nil, map[string]any{"callback_query_id": callbackID}, &result); err != nil {
		return err
	}
	if !result.OK {
		return c.apiError("answerCallbackQuery", result.ErrorCode, result.Description)
	}
	return nil
}
