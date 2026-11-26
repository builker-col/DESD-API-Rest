package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type Publisher struct {
	mu          sync.Mutex
	url         string
	dlxExchange string
	conn        *amqp.Connection
	ch          *amqp.Channel
	declared    map[string]bool
	dlxReady    bool
}

func NewPublisher(ctx context.Context, host, user, pass, dlxExchange string) (*Publisher, error) {
	url := fmt.Sprintf("amqp://%s:%s@%s:5672/", user, pass, host)
	if dlxExchange == "" {
		dlxExchange = "desd.dlx"
	}
	p := &Publisher{
		url:         url,
		dlxExchange: dlxExchange,
		declared:    make(map[string]bool),
	}
	if err := p.connectWithRetry(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Publisher) connectWithRetry(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= 20; attempt++ {
		conn, err := amqp.DialConfig(p.url, amqp.Config{
			Heartbeat: 60 * time.Second,
			Dial:      amqp.DefaultDial(10 * time.Second),
		})
		if err == nil {
			ch, chErr := conn.Channel()
			if chErr == nil {
				p.conn = conn
				p.ch = ch
				p.declared = make(map[string]bool)
				p.dlxReady = false
				return nil
			}
			_ = conn.Close()
			lastErr = chErr
		} else {
			lastErr = err
		}
		fmt.Printf("[api] Esperando RabbitMQ (intento %d/20): %v\n", attempt, lastErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("no se pudo conectar a RabbitMQ: %w", lastErr)
}

func (p *Publisher) ensureOpen() error {
	if p.conn != nil && !p.conn.IsClosed() && p.ch != nil && !p.ch.IsClosed() {
		return nil
	}
	conn, err := amqp.DialConfig(p.url, amqp.Config{
		Heartbeat: 60 * time.Second,
		Dial:      amqp.DefaultDial(10 * time.Second),
	})
	if err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return err
	}
	p.conn = conn
	p.ch = ch
	p.declared = make(map[string]bool)
	p.dlxReady = false
	return nil
}

func (p *Publisher) declareQueueWithDLQ(queueName string) error {
	if !p.dlxReady {
		if err := p.ch.ExchangeDeclare(p.dlxExchange, "direct", true, false, false, false, nil); err != nil {
			return err
		}
		p.dlxReady = true
	}

	dlqName := queueName + "_dlq"
	if _, err := p.ch.QueueDeclare(dlqName, true, false, false, false, nil); err != nil {
		return err
	}
	if err := p.ch.QueueBind(dlqName, queueName, p.dlxExchange, false, nil); err != nil {
		return err
	}

	args := amqp.Table{
		"x-dead-letter-exchange":    p.dlxExchange,
		"x-dead-letter-routing-key": queueName,
	}
	if _, err := p.ch.QueueDeclare(queueName, true, false, false, false, args); err != nil {
		return err
	}
	p.declared[queueName] = true
	return nil
}

func (p *Publisher) IsHealthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn != nil && !p.conn.IsClosed() && p.ch != nil && !p.ch.IsClosed()
}

func (p *Publisher) PublishJSON(ctx context.Context, queueName string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.ensureOpen(); err != nil {
		return err
	}
	if !p.declared[queueName] {
		if err := p.declareQueueWithDLQ(queueName); err != nil {
			return err
		}
	}
	return p.ch.PublishWithContext(ctx, "", queueName, false, false, amqp.Publishing{
		DeliveryMode: amqp.Persistent,
		ContentType:  "application/json",
		Body:         body,
	})
}

func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch != nil {
		_ = p.ch.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
}
