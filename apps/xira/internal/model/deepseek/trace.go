package deepseek

import (
	"context"
	"net/http"
	"time"
)

type requestTraceRecorderKey struct{}

type RequestTraceRecorder func(context.Context, ChatRequest)

func WithRequestTraceRecorder(ctx context.Context, recorder RequestTraceRecorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, requestTraceRecorderKey{}, recorder)
}

func traceRequest(ctx context.Context, req ChatRequest) {
	recorder, ok := ctx.Value(requestTraceRecorderKey{}).(RequestTraceRecorder)
	if !ok || recorder == nil {
		return
	}
	recorder(ctx, req)
}

type callTraceRecorderKey struct{}

type CallTrace struct {
	Request   ChatRequest
	Response  *ChatResponse
	Err       error
	StartedAt time.Time
	EndedAt   time.Time
}

type CallTraceRecorder func(context.Context, CallTrace)

func WithCallTraceRecorder(ctx context.Context, recorder CallTraceRecorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, callTraceRecorderKey{}, recorder)
}

func traceCall(ctx context.Context, call CallTrace) {
	recorder, ok := ctx.Value(callTraceRecorderKey{}).(CallTraceRecorder)
	if !ok || recorder == nil {
		return
	}
	recorder(ctx, call)
}

type rawTraceRecorderKey struct{}

type RawTrace struct {
	Event      string
	Method     string
	URL        string
	Body       []byte
	StatusCode int
	Header     http.Header
	Error      string
}

type RawTraceRecorder func(context.Context, RawTrace)

func WithRawTraceRecorder(ctx context.Context, recorder RawTraceRecorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, rawTraceRecorderKey{}, recorder)
}

func traceRaw(ctx context.Context, trace RawTrace) {
	recorder, ok := ctx.Value(rawTraceRecorderKey{}).(RawTraceRecorder)
	if !ok || recorder == nil {
		return
	}
	recorder(ctx, trace)
}
