// Package queue defines the TaskQueue interface and provides the RabbitMQ implementation.
// The queue is responsible for task dispatch and flow control only — it does not store
// task state or results. That responsibility belongs to the store package.
package queue

import (
	"context"
	"encoding/json"
)

// TaskMessage is the unit of work passed through the queue.
type TaskMessage struct {
	TaskID   string          `json:"task_id"`
	Input    json.RawMessage `json:"input"`
	Priority uint8           `json:"priority,omitempty"` // 0 = default (lowest), higher = processed first
}

// Delivery wraps a TaskMessage with acknowledgement controls.
// The worker must call Ack() after successful processing or Nack() on failure.
// If neither is called (e.g. worker crashes), the message remains unacknowledged
// and RabbitMQ will redeliver it to another consumer.
type Delivery struct {
	Message TaskMessage
	ack     func() error
	nack    func(requeue bool) error
}

// Ack acknowledges the message, removing it from the queue.
func (d *Delivery) Ack() error {
	if d.ack != nil {
		return d.ack()
	}
	return nil
}

// Nack negatively acknowledges the message.
// If requeue is true, the message is returned to the queue for redelivery.
// If requeue is false, the message is discarded (or routed to the dead-letter queue).
func (d *Delivery) Nack(requeue bool) error {
	if d.nack != nil {
		return d.nack(requeue)
	}
	return nil
}

// TaskQueue abstracts the mechanism used to dispatch tasks from the HTTP layer
// to the worker pool.
type TaskQueue interface {
	// Enqueue publishes a task message to the queue.
	Enqueue(ctx context.Context, msg TaskMessage) error

	// Dequeue blocks until a delivery is available or the context is cancelled.
	// The caller MUST call Ack() or Nack() on the returned Delivery.
	Dequeue(ctx context.Context) (*Delivery, error)

	// Close releases any resources held by the queue (connections, channels, etc.).
	Close() error
}
