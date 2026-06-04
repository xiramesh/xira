package runtime

import (
	"context"
	"sync"
)

type EventBus struct {
	mu     sync.RWMutex
	subs   map[chan RuntimeEvent]struct{}
	closed bool
}

func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[chan RuntimeEvent]struct{})}
}

func (b *EventBus) Publish(evt RuntimeEvent) {
	if b == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for ch := range b.subs {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (b *EventBus) Subscribe(ctx context.Context) <-chan RuntimeEvent {
	ch := make(chan RuntimeEvent, 64)
	b.mu.Lock()
	if b.closed {
		close(ch)
		b.mu.Unlock()
		return ch
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}()
	return ch
}

func (b *EventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		close(ch)
		delete(b.subs, ch)
	}
}
