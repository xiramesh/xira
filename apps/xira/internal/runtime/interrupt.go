package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/xiramesh/xira/internal/humanrequest"
)

const StatusWaitingHuman = "waiting_human"

var errRuntimeInterrupted = errors.New("runtime interrupted")

type runtimeSuspendCollector struct {
	mu                 sync.Mutex
	humanRequests      []humanrequest.HumanRequest
	blockedBy          []BlockedBy
	suspendedToolCalls []SuspendedToolCall
}

type runtimeSuspendCollectorKey struct{}
type runtimeInterruptCancelKey struct{}

func contextWithRuntimeSuspendCollector(ctx context.Context, collector *runtimeSuspendCollector) context.Context {
	return context.WithValue(ctx, runtimeSuspendCollectorKey{}, collector)
}

func runtimeSuspendCollectorFromContext(ctx context.Context) *runtimeSuspendCollector {
	collector, _ := ctx.Value(runtimeSuspendCollectorKey{}).(*runtimeSuspendCollector)
	return collector
}

func contextWithRuntimeInterruptCancel(ctx context.Context, cancel context.CancelFunc) context.Context {
	return context.WithValue(ctx, runtimeInterruptCancelKey{}, cancel)
}

func cancelRuntimeOnInterrupt(ctx context.Context) {
	cancel, _ := ctx.Value(runtimeInterruptCancelKey{}).(context.CancelFunc)
	if cancel != nil {
		cancel()
	}
}

func newRuntimeSuspendCollector() *runtimeSuspendCollector {
	return &runtimeSuspendCollector{}
}

func (c *runtimeSuspendCollector) AddHumanRequest(req humanrequest.HumanRequest, reason string) {
	c.addHumanRequest(req, "human_request", reason)
}

func (c *runtimeSuspendCollector) addHumanRequest(req humanrequest.HumanRequest, blockedType, reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.humanRequests = append(c.humanRequests, req)
	c.blockedBy = append(c.blockedBy, BlockedBy{
		Type:           blockedType,
		HumanRequestID: req.ID,
		RunID:          req.RunID,
		ToolCallID:     req.ToolCallID,
		Reason:         reason,
	})
}

func (c *runtimeSuspendCollector) SuspendToolCall(call SuspendedToolCall) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.suspendedToolCalls = append(c.suspendedToolCalls, call)
}

func (c *runtimeSuspendCollector) HasInterrupt() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.humanRequests) > 0 || len(c.blockedBy) > 0 || len(c.suspendedToolCalls) > 0
}

func (c *runtimeSuspendCollector) Interrupt() *RunInterrupt {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.humanRequests) == 0 && len(c.blockedBy) == 0 && len(c.suspendedToolCalls) == 0 {
		return nil
	}
	return &RunInterrupt{
		Status:             StatusWaitingHuman,
		Reason:             interruptReason(c.blockedBy),
		HumanRequests:      append([]humanrequest.HumanRequest(nil), c.humanRequests...),
		BlockedBy:          append([]BlockedBy(nil), c.blockedBy...),
		SuspendedToolCalls: append([]SuspendedToolCall(nil), c.suspendedToolCalls...),
	}
}

func interruptReason(blocked []BlockedBy) string {
	if len(blocked) == 0 {
		return ""
	}
	return blocked[0].Type
}

func requestKindFromString(value string) (humanrequest.RequestKind, error) {
	switch humanrequest.RequestKind(value) {
	case humanrequest.RequestFreeform:
		return humanrequest.RequestFreeform, nil
	case humanrequest.RequestApproval:
		return humanrequest.RequestApproval, nil
	default:
		return "", fmt.Errorf("unsupported human request kind %q", value)
	}
}
