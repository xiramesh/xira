package deepseek

import "context"

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
