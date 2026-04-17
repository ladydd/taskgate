package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// DefaultPriority is the default message priority (0 = lowest).
const DefaultPriority = 0

// RabbitMQQueue implements TaskQueue using RabbitMQ.
//
// Features:
//   - Durable queue with persistent messages — survives broker restarts
//   - Manual acknowledgement — messages are not lost if a worker crashes
//   - Priority queue — higher priority tasks are delivered first
//   - Dead-letter exchange (DLX) — failed/rejected messages are routed to a
//     dead-letter queue for inspection, retry, or alerting
//   - Automatic reconnection — recovers from broker restarts and network blips
type RabbitMQQueue struct {
	cfg        Config
	mu         sync.Mutex
	conn       *amqp.Connection
	pubCh      *amqp.Channel
	consCh     *amqp.Channel
	deliveries <-chan amqp.Delivery
	closed     chan struct{}
}

// Config holds the configuration for connecting to RabbitMQ.
type Config struct {
	URL             string // AMQP connection URL
	QueueName       string // Main task queue name
	PrefetchCount   int    // Unacknowledged messages per consumer
	MaxPriority     int    // Max priority level (0 = disabled)
	DeadLetterQueue string // Dead-letter queue name (empty = disabled)
}

// NewRabbitMQQueue connects to RabbitMQ, declares the queue, and starts consuming.
func NewRabbitMQQueue(cfg Config) (*RabbitMQQueue, error) {
	q := &RabbitMQQueue{
		cfg:    cfg,
		closed: make(chan struct{}),
	}

	if err := q.connect(); err != nil {
		return nil, err
	}

	return q, nil
}

// connect establishes the AMQP connection, channels, and starts consuming.
func (q *RabbitMQQueue) connect() error {
	conn, err := amqp.Dial(q.cfg.URL)
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	pubCh, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to open publish channel: %w", err)
	}

	consCh, err := conn.Channel()
	if err != nil {
		pubCh.Close()
		conn.Close()
		return fmt.Errorf("failed to open consume channel: %w", err)
	}

	// Dead-letter exchange and queue.
	if q.cfg.DeadLetterQueue != "" {
		if err := declareDLX(consCh, q.cfg); err != nil {
			consCh.Close()
			pubCh.Close()
			conn.Close()
			return err
		}
	}

	// Queue arguments.
	queueArgs := amqp.Table{}
	if q.cfg.MaxPriority > 0 {
		queueArgs["x-max-priority"] = int32(q.cfg.MaxPriority)
	}
	if q.cfg.DeadLetterQueue != "" {
		dlxName := q.cfg.QueueName + ".dlx"
		queueArgs["x-dead-letter-exchange"] = dlxName
		queueArgs["x-dead-letter-routing-key"] = q.cfg.DeadLetterQueue
	}

	// Declare queue on both channels.
	for _, ch := range []*amqp.Channel{consCh, pubCh} {
		if _, err := ch.QueueDeclare(
			q.cfg.QueueName, true, false, false, false, queueArgs,
		); err != nil {
			consCh.Close()
			pubCh.Close()
			conn.Close()
			return fmt.Errorf("failed to declare queue %q: %w", q.cfg.QueueName, err)
		}
	}

	// Prefetch.
	prefetch := q.cfg.PrefetchCount
	if prefetch <= 0 {
		prefetch = 1
	}
	if err := consCh.Qos(prefetch, 0, false); err != nil {
		consCh.Close()
		pubCh.Close()
		conn.Close()
		return fmt.Errorf("failed to set QoS: %w", err)
	}

	deliveries, err := consCh.Consume(
		q.cfg.QueueName, "", false, false, false, false, nil,
	)
	if err != nil {
		consCh.Close()
		pubCh.Close()
		conn.Close()
		return fmt.Errorf("failed to start consuming: %w", err)
	}

	q.mu.Lock()
	q.conn = conn
	q.pubCh = pubCh
	q.consCh = consCh
	q.deliveries = deliveries
	q.mu.Unlock()

	slog.Info("connected to RabbitMQ",
		"url", q.cfg.URL,
		"queue", q.cfg.QueueName,
		"prefetch", prefetch,
		"max_priority", q.cfg.MaxPriority,
		"dead_letter_queue", q.cfg.DeadLetterQueue,
	)

	return nil
}

// reconnect attempts to re-establish the connection with exponential backoff.
func (q *RabbitMQQueue) reconnect() error {
	// Close old resources silently.
	q.mu.Lock()
	if q.consCh != nil {
		q.consCh.Close()
	}
	if q.pubCh != nil {
		q.pubCh.Close()
	}
	if q.conn != nil {
		q.conn.Close()
	}
	q.mu.Unlock()

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-q.closed:
			return fmt.Errorf("queue closed during reconnect")
		default:
		}

		slog.Info("attempting RabbitMQ reconnect", "backoff", backoff.String())
		if err := q.connect(); err != nil {
			slog.Error("RabbitMQ reconnect failed", "error", err)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		return nil
	}
}

// declareDLX creates the dead-letter exchange and queue.
func declareDLX(ch *amqp.Channel, cfg Config) error {
	dlxName := cfg.QueueName + ".dlx"

	if err := ch.ExchangeDeclare(dlxName, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("failed to declare dead-letter exchange %q: %w", dlxName, err)
	}

	if _, err := ch.QueueDeclare(cfg.DeadLetterQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("failed to declare dead-letter queue %q: %w", cfg.DeadLetterQueue, err)
	}

	if err := ch.QueueBind(cfg.DeadLetterQueue, cfg.DeadLetterQueue, dlxName, false, nil); err != nil {
		return fmt.Errorf("failed to bind dead-letter queue: %w", err)
	}

	return nil
}

// Enqueue publishes a task message to the RabbitMQ queue.
func (q *RabbitMQQueue) Enqueue(ctx context.Context, msg TaskMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal task message: %w", err)
	}

	q.mu.Lock()
	ch := q.pubCh
	q.mu.Unlock()

	return ch.PublishWithContext(ctx,
		"", q.cfg.QueueName, false, false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Priority:     msg.Priority,
			Body:         body,
		},
	)
}

// Dequeue blocks until a delivery is available or the context is cancelled.
// The caller MUST call Ack() after successful processing or Nack() on failure.
// If the worker crashes without calling either, RabbitMQ will redeliver the message.
func (q *RabbitMQQueue) Dequeue(ctx context.Context) (*Delivery, error) {
	q.mu.Lock()
	deliveries := q.deliveries
	q.mu.Unlock()

	select {
	case d, ok := <-deliveries:
		if !ok {
			// Channel closed — attempt reconnect.
			if err := q.reconnect(); err != nil {
				return nil, err
			}
			// Retry after reconnect.
			return q.Dequeue(ctx)
		}

		var msg TaskMessage
		if err := json.Unmarshal(d.Body, &msg); err != nil {
			_ = d.Nack(false, false) // → DLQ
			return nil, fmt.Errorf("failed to unmarshal task message: %w", err)
		}

		return &Delivery{
			Message: msg,
			ack:     func() error { return d.Ack(false) },
			nack:    func(requeue bool) error { return d.Nack(false, requeue) },
		}, nil

	case <-ctx.Done():
		return nil, fmt.Errorf("dequeue cancelled: %w", ctx.Err())
	}
}

// Close cleanly shuts down the RabbitMQ connection.
func (q *RabbitMQQueue) Close() error {
	close(q.closed)

	q.mu.Lock()
	defer q.mu.Unlock()

	var firstErr error
	if q.consCh != nil {
		if err := q.consCh.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if q.pubCh != nil {
		if err := q.pubCh.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if q.conn != nil {
		if err := q.conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
