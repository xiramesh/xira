package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// human_interpret.go: #107 — agent 理解用户意图后的合规 resolve 路径。
//
// 设计基线(#105):agent 产出"意图",不直接 resolve;resolve 执行权在
// runtime,且 runtime 回查 pending 校验。本工具镜像 answer_child 的异步
// resolve 模式(executeAnswerChild, answer_child.go),但加四项校验:
//
//  1. request 存在且 pending(防伪造 request_id / injection)
//  2. source 允许(agent_request / flow_human_approval 允许;其他拒)
//  3. chatKey 匹配(HR.ChatKey == 当前 turn chatKey,防跨 chat 注入)
//  4. 无歧义(单次一个 request_id;signal 合法)
//
// 校验在同步阶段做(给模型即时反馈),通过才进异步 resolve。绝不嵌套 turn
// (硬约束):resolve 在 detached goroutine + context.Background() 跑,本工具
// 立即返回 {status:"interpreting"}。resolve 触发的 resume final 由
// deliverResumeFinal 异步投回 IM,不依赖当前 turn。
//
// 为什么不是 human.respond(文档 xira-agent-hitl-v0.zh.md:1304 明禁):
// human.respond = 模型直接 resolve,可被 prompt injection 滥用(模型自己
// 批准自己)。human.interpret 不 resolve——它只声明意图并触发 runtime 层的
// resolve;runtime 回查 pending 列表,模型即使被 injection 报一个不存在的
// request_id,校验 1 直接拒。信任分层保住。

const humanInterpretToolName = "human.interpret"

// humanInterpretInput 是 agent 调 human.interpret 时的参数。
type humanInterpretInput struct {
	RequestID string // pending HumanRequest 的 id(来自 #106 注入的 summary)
	Signal    string // approve / deny / answer / cancel
	Reasoning string // 可选,agent 的理解依据(审计用)
	ChatKey   string // 当前 turn 的 chatKey(由 runtime 填,不从模型 args 取——防伪造)
}

// humanInterpretInputSchema 是 ADK input schema。ChatKey 不暴露给模型——
// 它由 runtime 从当前 turn ctx 填,模型无法伪造跨 chat 的 chatKey。
func humanInterpretInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"request_id": {Type: "string", Description: "The id of the pending HumanRequest this reply answers (from the Pending Human Requests summary)."},
			"signal": {
				Type:        "string",
				Description: "The user's intent: approve, deny, answer, or cancel.",
				Enum:        []any{"approve", "deny", "answer", "cancel"},
			},
			"reasoning": {Type: "string", Description: "Optional: why you read the user's reply as this signal for this request."},
		},
		Required:             []string{"request_id", "signal"},
		AdditionalProperties: rejectAllSchema(),
	}
}

// sanitizeHumanInterpretInput 从模型 args 抽 {request_id, signal, reasoning}。
// ChatKey 不从 args 取(由 runtime 填)。
func sanitizeHumanInterpretInput(args map[string]any) humanInterpretInput {
	spec := humanInterpretInput{}
	for k, v := range args {
		switch k {
		case "request_id":
			spec.RequestID = strings.TrimSpace(fmt.Sprint(v))
		case "signal":
			// Lowercase: models may emit "Approve"/"APPROVE"; ResponseKind
			// constants are lowercase. Normalize before validation so we don't
			// reject a semantically-correct signal over casing.
			spec.Signal = strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
		case "reasoning":
			spec.Reasoning = strings.TrimSpace(fmt.Sprint(v))
		}
	}
	return spec
}

// validateHumanInterpret 做四项校验。返回 nil 通过,非 nil error 带
// 可读原因(供模型/审计看)。这是 #107 的核心安全契约,追求 100% 覆盖。
func validateHumanInterpret(ctx context.Context, s *Service, in humanInterpretInput) (*humanrequest.HumanRequest, error) {
	if strings.TrimSpace(in.RequestID) == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	if !isValidInterpretSignal(in.Signal) {
		return nil, fmt.Errorf("signal %q is not a valid interpret signal (approve/deny/answer/cancel)", in.Signal)
	}
	// 校验 1: request 存在 + pending(同步,给模型即时反馈;防伪造 id)。
	existing, err := s.GetHumanRequest(ctx, in.RequestID)
	if err != nil || existing == nil {
		return nil, fmt.Errorf("human request %q not found", in.RequestID)
	}
	if existing.Status != humanrequest.StatusPending {
		return nil, fmt.Errorf("human request %q is already %s, cannot interpret", in.RequestID, existing.Status)
	}
	// 校验 2: source 白名单——agent_request / flow_human_approval 允许;
	// 其他未知 source 拒。
	if existing.Source != "agent_request" && existing.Source != "flow_human_approval" {
		return nil, fmt.Errorf("human request %q has unsupported source %q for interpret", in.RequestID, existing.Source)
	}
	// 校验 3: chatKey 匹配(防跨 chat 注入)。HR.ChatKey 必须等于当前 turn。
	hrChatKey := strings.TrimSpace(existing.ChatKey)
	turnChatKey := strings.TrimSpace(in.ChatKey)
	if hrChatKey == "" {
		// HR 没记 chatKey(老数据或 flow #112 gap)——保守拒,不跨 chat 猜。
		return nil, fmt.Errorf("human request %q has no chat_key; cannot verify it belongs to this chat", in.RequestID)
	}
	if hrChatKey != turnChatKey {
		return nil, fmt.Errorf("human request %q belongs to chat %q, not this chat %q", in.RequestID, hrChatKey, turnChatKey)
	}
	// 校验 4: 无歧义——单次一个 request_id(schema 已保证,这里 defense-in-depth)。
	// 多个 pending 需多次调用 interpret,每次一个。
	return existing, nil
}

func isValidInterpretSignal(s string) bool {
	switch humanrequest.ResponseKind(s) {
	case humanrequest.ResponseApprove, humanrequest.ResponseDeny,
		humanrequest.ResponseAnswer, humanrequest.ResponseCancel:
		return true
	}
	return false
}

// executeHumanInterpret 是核心逻辑,与 ADK tool wrapper 分离便于测试。
// 镜像 executeAnswerChild:同步预校验 → 异步 resolve(detached goroutine)
// → 立即返回 {status:"interpreting"}。绝不阻塞 turn / 绝不嵌套 resume。
func executeHumanInterpret(ctx context.Context, s *Service, in humanInterpretInput) map[string]any {
	hr, err := validateHumanInterpret(ctx, s, in)
	if err != nil {
		return map[string]any{"status": "rejected", "error": err.Error()}
	}
	requestID := hr.ID
	signal := in.Signal
	reasoning := in.Reasoning
	resolve := func() {
		// context.Background():当前 turn 可能在本工具返回后立即结束;
		// resolve 触发的 resume(一次完整 generate)必须比当前 turn 长命。
		// 对称 answer_child.go:117-128 和 spawn_turn.go:108-118。
		defer func() {
			if r := recover(); r != nil {
				slog.Error("human.interpret async resolve panicked",
					"human_request_id", requestID, "panic", r)
			}
		}()
		_, rerr := s.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
			Kind:    humanrequest.ResponseKind(signal),
			Actor:   "agent_interpret",
			Message: reasoning,
		})
		if rerr != nil {
			// HR 已 resolved(store 层,在 resume 前)。resume 的 model-call
			// 错误通过 run status 暴露;这里 log,不吞。
			slog.Warn("human.interpret async resolve finished with error",
				"human_request_id", requestID, "error", rerr)
		}
	}
	go resolve()

	return map[string]any{
		"status":           "interpreting",
		"human_request_id": requestID,
		"signal":           signal,
		"note":             "request resolved in the background; the resumed run's final routes back to this chat when done",
	}
}
