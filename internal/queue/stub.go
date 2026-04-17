package queue

import (
	"context"
	"fmt"
)

// StubQueue is a minimal in-process TaskQueue for use in tests.
// It is backed by a Go channel and requires no external dependencies.
type StubQueue struct {
	ch chan TaskMessage
}

// NewStubQueue creates a StubQueue with the given buffer size.
func NewStubQueue(size int) *StubQueue {
	return &StubQueue{ch: make(chan TaskMessage, size)}
}

// Enqueue sends a task message into the channel.
func (q *StubQueue) Enqueue(ctx context.Context, msg TaskMessage) error {
	select {
	case q.ch <- msg:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("enqueue cancelled: %w", ctx.Err())
	}
}

// Dequeue blocks until a task message is available or the context is cancelled.
func (q *StubQueue) Dequeue(ctx context.Context) (*Delivery, error) {
	select {
	case msg, ok := <-q.ch:
		if !ok {
			return nil, fmt.Errorf("queue closed")
		}
		return &Delivery{
			Message: msg,
			ack:     func() error { return nil },
			nack:    func(requeue bool) error { return nil },
		}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("dequeue cancelled: %w", ctx.Err())
	}
}

// Close closes the underlying channel.
func (q *StubQueue) Close() error {
	close(q.ch)
	return nil
}
