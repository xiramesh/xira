package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	frt "github.com/xiramesh/xira/internal/runtime"
)

type humanCardAction struct {
	RequestID        string
	CorrelationToken string
	Kind             humanrequest.ResponseKind
	Answer           string
}

func buildHumanRequestCard(req humanrequest.HumanRequest) (string, error) {
	if strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.Question) == "" || strings.TrimSpace(req.CorrelationToken) == "" {
		return "", fmt.Errorf("feishu HumanRequest card requires id, question, and correlation token")
	}
	elements := []map[string]any{{"tag": "markdown", "content": req.Question}}
	switch req.Kind {
	case humanrequest.RequestApproval:
		elements = append(elements,
			humanRequestButton("approve_btn", "批准", "primary", req, humanrequest.ResponseApprove, ""),
			humanRequestButton("deny_btn", "拒绝", "danger", req, humanrequest.ResponseDeny, ""),
		)
	case humanrequest.RequestFreeform:
		if len(req.Options) == 0 {
			ref, err := humanrequest.TextReference(req.CorrelationToken)
			if err != nil {
				return "", err
			}
			elements = append(elements, map[string]any{"tag": "markdown", "content": "请回复：`/answer " + ref + " <你的回答>`"})
		} else {
			for i, option := range req.Options {
				elements = append(elements, humanRequestButton(fmt.Sprintf("option_%d", i+1), option.Label, "default", req, humanrequest.ResponseAnswer, option.ID))
			}
		}
	default:
		return "", fmt.Errorf("unsupported HumanRequest kind %q", req.Kind)
	}
	return marshalHumanRequestCard("需要你的确认", "blue", elements)
}

func buildResolvedHumanRequestCard(req humanrequest.HumanRequest) (string, error) {
	if req.Response == nil {
		return "", fmt.Errorf("resolved Feishu HumanRequest card requires a persisted response")
	}
	kind := req.Response.Kind
	answer := req.Response.Message
	status := "已处理"
	template := "green"
	switch kind {
	case humanrequest.ResponseApprove:
		status = "已批准"
	case humanrequest.ResponseDeny:
		status, template = "已拒绝", "red"
	case humanrequest.ResponseCancel:
		status, template = "已取消", "grey"
	case humanrequest.ResponseAnswer:
		status = "已回答"
	default:
		return "", fmt.Errorf("unsupported HumanResponse kind %q", kind)
	}
	elements := []map[string]any{
		{"tag": "markdown", "content": req.Question},
		{"tag": "markdown", "content": "**" + status + "**"},
	}
	if strings.TrimSpace(answer) != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": strings.TrimSpace(answer)})
	}
	return marshalHumanRequestCard(status, template, elements)
}

func humanRequestButton(elementID, label, buttonType string, req humanrequest.HumanRequest, kind humanrequest.ResponseKind, answer string) map[string]any {
	value := map[string]any{
		"request_id": req.ID, "correlation_token": req.CorrelationToken, "action": string(kind),
	}
	if strings.TrimSpace(answer) != "" {
		value["answer"] = strings.TrimSpace(answer)
	}
	return map[string]any{
		"tag": "button", "element_id": elementID, "type": buttonType,
		"text":      map[string]any{"tag": "plain_text", "content": label},
		"behaviors": []map[string]any{{"type": "callback", "value": value}},
	}
}

func marshalHumanRequestCard(title, template string, elements []map[string]any) (string, error) {
	data, err := json.Marshal(map[string]any{
		"schema": "2.0", "config": map[string]any{"update_multi": true},
		"header": map[string]any{"template": template, "title": map[string]any{"tag": "plain_text", "content": title}},
		"body":   map[string]any{"elements": elements},
	})
	if err != nil {
		return "", fmt.Errorf("marshal Feishu HumanRequest card: %w", err)
	}
	return string(data), nil
}

func (r *Runner) ValidateHumanRequestDelivery(target frt.HumanRequestDeliveryTarget) error {
	_, _, err := r.humanRequestDeliveryRoute(target)
	return err
}

func (r *Runner) DeliverHumanRequest(ctx context.Context, req humanrequest.HumanRequest, target frt.HumanRequestDeliveryTarget) (frt.HumanRequestDeliveryReceipt, error) {
	receiveIDType, receiveID, err := r.humanRequestDeliveryRoute(target)
	if err != nil {
		return frt.HumanRequestDeliveryReceipt{}, err
	}
	cardContent, cardErr := buildHumanRequestCard(req)
	if cardErr == nil {
		messageID, sendErr := r.createMessage(ctx, receiveIDType, receiveID, larkim.MsgTypeInteractive, cardContent)
		if sendErr == nil {
			return frt.HumanRequestDeliveryReceipt{MessageID: messageID}, nil
		}
		cardErr = sendErr
	}
	slog.Warn("feishu HumanRequest card failed; falling back to explicit text", "entrypoint_id", r.definition.ID, "request_id", req.ID, "error", cardErr)
	text, err := humanrequest.RenderTextRequest(req)
	if err != nil {
		return frt.HumanRequestDeliveryReceipt{}, err
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return frt.HumanRequestDeliveryReceipt{}, err
	}
	messageID, err := r.createMessage(ctx, receiveIDType, receiveID, larkim.MsgTypeText, string(content))
	if err != nil {
		return frt.HumanRequestDeliveryReceipt{}, err
	}
	return frt.HumanRequestDeliveryReceipt{MessageID: messageID}, nil
}

// humanRequestDeliveryRoute is the sealed Feishu recipient boundary.
// coverage: contract (100% required)
func (r *Runner) humanRequestDeliveryRoute(target frt.HumanRequestDeliveryTarget) (string, string, error) {
	if r == nil {
		return "", "", fmt.Errorf("feishu runner is not configured")
	}
	route := target.Route
	route.Channel = strings.ToLower(strings.TrimSpace(route.Channel))
	route.EntrypointID = strings.TrimSpace(route.EntrypointID)
	route.ChatID = strings.TrimSpace(route.ChatID)
	if route.EntrypointID != strings.TrimSpace(r.definition.ID) {
		return "", "", fmt.Errorf("feishu HumanRequest target entrypoint %q does not match runner %q", route.EntrypointID, r.definition.ID)
	}
	if route.Channel != "feishu" {
		return "", "", fmt.Errorf("feishu HumanRequest target has channel %q", route.Channel)
	}
	if target.Recipient == nil {
		if route.ChatID == "" {
			return "", "", fmt.Errorf("feishu HumanRequest target has no chat_id")
		}
		return larkim.ReceiveIdTypeChatId, route.ChatID, nil
	}
	typed := target.Recipient.Normalize()
	receiveIDType, err := feishuReceiveIDType(typed.IDType)
	if err != nil {
		return "", "", fmt.Errorf("feishu HumanRequest recipient: %w", err)
	}
	if typed.ID == "" {
		return "", "", fmt.Errorf("feishu HumanRequest recipient has no id")
	}
	return receiveIDType, typed.ID, nil
}

// handleCardAction parses only authenticated SDK callback facts and commits an
// exact response before returning a replacement card.
func (r *Runner) handleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	command, identities, messageID, err := parseHumanCardAction(event)
	if err != nil || r == nil || r.asyncExactResolver == nil {
		return cardErrorResponse(), nil
	}
	input := humanrequest.HumanResponseEnvelope{
		RequestID: command.RequestID, CorrelationToken: command.CorrelationToken,
		EntrypointID: strings.TrimSpace(r.definition.ID), SenderIdentities: identities,
		DeliveryMessageID: messageID, Kind: command.Kind, Message: command.Answer,
		IdempotencyKey: cardActionIdempotencyKey(r.definition.ID, messageID, identities, command),
	}
	resolved, err := r.asyncExactResolver.ResolveHumanResponseAsync(ctx, input)
	if err != nil || resolved == nil {
		slog.Warn("feishu HumanRequest card action rejected", "entrypoint_id", r.definition.ID, "request_id", command.RequestID, "message_id", messageID, "error", err)
		return cardErrorResponse(), nil
	}
	content, err := buildResolvedHumanRequestCard(*resolved)
	if err != nil {
		return cardErrorResponse(), nil
	}
	var cardData map[string]any
	if err := json.Unmarshal([]byte(content), &cardData); err != nil {
		return cardErrorResponse(), nil
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已收到，正在继续处理。"},
		Card:  &callback.Card{Type: "card_json", Data: cardData},
	}, nil
}

func parseHumanCardAction(event *callback.CardActionTriggerEvent) (humanCardAction, map[string]string, string, error) {
	if event == nil || event.Event == nil || event.Event.Operator == nil || event.Event.Action == nil || event.Event.Context == nil {
		return humanCardAction{}, nil, "", fmt.Errorf("incomplete Feishu card action")
	}
	if event.Event.Action.Tag != "button" {
		return humanCardAction{}, nil, "", fmt.Errorf("unsupported Feishu card action tag %q", event.Event.Action.Tag)
	}
	identities := map[string]string{}
	if event.Event.Operator.UserID != nil && strings.TrimSpace(*event.Event.Operator.UserID) != "" {
		identities["user_id"] = strings.TrimSpace(*event.Event.Operator.UserID)
	}
	if strings.TrimSpace(event.Event.Operator.OpenID) != "" {
		identities["open_id"] = strings.TrimSpace(event.Event.Operator.OpenID)
	}
	messageID := strings.TrimSpace(event.Event.Context.OpenMessageID)
	requestID, requestOK := stringCardActionValue(event.Event.Action.Value, "request_id")
	correlation, correlationOK := stringCardActionValue(event.Event.Action.Value, "correlation_token")
	action, actionOK := stringCardActionValue(event.Event.Action.Value, "action")
	answer, _ := stringCardActionValue(event.Event.Action.Value, "answer")
	if len(identities) == 0 || messageID == "" || !requestOK || !correlationOK || !actionOK {
		return humanCardAction{}, nil, "", fmt.Errorf("Feishu card action is missing authority fields")
	}
	command := humanCardAction{RequestID: requestID, CorrelationToken: correlation, Answer: answer}
	switch humanrequest.ResponseKind(strings.ToLower(action)) {
	case humanrequest.ResponseApprove:
		command.Kind = humanrequest.ResponseApprove
	case humanrequest.ResponseDeny:
		command.Kind = humanrequest.ResponseDeny
	case humanrequest.ResponseCancel:
		command.Kind = humanrequest.ResponseCancel
	case humanrequest.ResponseAnswer:
		if strings.TrimSpace(answer) == "" {
			return humanCardAction{}, nil, "", fmt.Errorf("Feishu answer action has no answer")
		}
		command.Kind = humanrequest.ResponseAnswer
	default:
		return humanCardAction{}, nil, "", fmt.Errorf("unsupported Feishu HumanResponse action %q", action)
	}
	return command, identities, messageID, nil
}

func stringCardActionValue(values map[string]any, key string) (string, bool) {
	value, ok := values[key].(string)
	value = strings.TrimSpace(value)
	return value, ok && value != ""
}

func cardActionIdempotencyKey(entrypointID, messageID string, identities map[string]string, command humanCardAction) string {
	parts := []string{entrypointID, messageID, identities["user_id"], identities["open_id"], command.RequestID, command.CorrelationToken, string(command.Kind), command.Answer}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "feishu-card:" + hex.EncodeToString(sum[:])
}

func cardErrorResponse() *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "error", Content: "无法接受该操作，请确认卡片、身份和请求状态。"}}
}

func (r *Runner) tryResolveTextHumanResponse(ctx context.Context, inbound channel.InboundContext, chatKey frt.ChatKey, content, dedupeKey string) bool {
	command, recognized, parseErr := humanrequest.ParseTextResponse(content)
	if !recognized {
		return false
	}
	responseText := "已收到回答。"
	committed := false
	if parseErr != nil || r.textResolver == nil {
		responseText = "回答格式无效，请使用请求中提供的完整 /answer 命令。"
	} else {
		_, err := r.textResolver.ResolveHumanTextResponse(ctx, humanrequest.TextResponseEnvelope{
			CorrelationToken: command.CorrelationToken, EntrypointID: inbound.EntrypointID,
			SenderID: inbound.SenderID, SenderIDType: inbound.SenderIDType,
			ChatKey: chatKey.String(), ChatType: inbound.ChatType, Answer: command.Answer,
			IdempotencyKey: "feishu:" + inbound.EntrypointID + ":" + inbound.MessageID,
		})
		if err != nil {
			slog.Warn("feishu text HumanResponse rejected", "entrypoint_id", inbound.EntrypointID, "message_id", inbound.MessageID, "error", err)
			responseText = "无法接受该回答。请确认请求编号、回答选项和回复身份。"
		} else {
			committed = true
		}
	}
	if committed {
		r.messages.Complete(dedupeKey, time.Now())
	}
	if err := r.sendTextTo(ctx, larkim.ReceiveIdTypeChatId, inbound.ChatID, responseText); err != nil {
		if !committed {
			r.messages.Forget(dedupeKey)
		}
		slog.Error("feishu HumanResponse acknowledgement failed", "entrypoint_id", inbound.EntrypointID, "message_id", inbound.MessageID, "error", err)
	} else if !committed {
		r.messages.Complete(dedupeKey, time.Now())
	}
	return true
}

var _ frt.HumanRequestDeliverer = (*Runner)(nil)
