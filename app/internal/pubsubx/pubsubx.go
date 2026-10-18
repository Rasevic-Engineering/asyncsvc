// Package pubsubx wraps Pub/Sub publishing and both delivery styles:
// push (Pub/Sub POSTs to an HTTP endpoint; used on Cloud Run) and streaming
// pull (we hold a connection open; used on GKE). Both are normalized into a
// Delivery so handlers don't care which one is in use.
package pubsubx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// Delivery is one message delivery, independent of push or pull.
type Delivery struct {
	Data       []byte
	Attributes map[string]string
	MessageID  string
	// 1-based. Only tracked by Pub/Sub when the subscription has a
	// dead-letter policy; otherwise always 1.
	Attempt      int
	Subscription string
}

// Handler processes a delivery. Returning nil acks; an error nacks, and
// Pub/Sub redelivers according to the subscription's retry policy.
type Handler func(context.Context, Delivery) error

// Publisher publishes to topics by ID, reusing one pubsub.Publisher per topic.
// Honors PUBSUB_EMULATOR_HOST for local development.
type Publisher struct {
	client *pubsub.Client

	mu   sync.Mutex
	pubs map[string]*pubsub.Publisher
}

func NewPublisher(ctx context.Context, project string) (*Publisher, error) {
	c, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("pubsub client: %w", err)
	}
	return &Publisher{client: c, pubs: map[string]*pubsub.Publisher{}}, nil
}

// Publish sends one message and waits for the server to acknowledge it.
func (p *Publisher) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) (string, error) {
	res := p.publisher(topic).Publish(ctx, &pubsub.Message{Data: data, Attributes: attrs})
	return res.Get(ctx)
}

func (p *Publisher) publisher(topic string) *pubsub.Publisher {
	p.mu.Lock()
	defer p.mu.Unlock()
	pub, ok := p.pubs[topic]
	if !ok {
		pub = p.client.Publisher(topic)
		p.pubs[topic] = pub
	}
	return pub
}

// Consume stream-pulls from a subscription until ctx is cancelled, then waits
// for in-flight handlers. The client keeps extending each message's ack
// deadline up to maxExtension, so handlers may run longer than the
// subscription's ack deadline.
func (p *Publisher) Consume(ctx context.Context, subscription string, maxOutstanding int, maxExtension time.Duration, h Handler) error {
	sub := p.client.Subscriber(subscription)
	sub.ReceiveSettings.MaxOutstandingMessages = maxOutstanding
	sub.ReceiveSettings.MaxExtension = maxExtension
	return sub.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
		d := Delivery{Data: m.Data, Attributes: m.Attributes, MessageID: m.ID, Attempt: 1, Subscription: subscription}
		if m.DeliveryAttempt != nil {
			d.Attempt = *m.DeliveryAttempt
		}
		if err := h(ctx, d); err != nil {
			m.Nack()
			return
		}
		m.Ack()
	})
}

// Close flushes pending messages and closes the client.
func (p *Publisher) Close() error {
	p.mu.Lock()
	for _, pub := range p.pubs {
		pub.Stop()
	}
	p.mu.Unlock()
	return p.client.Close()
}

// PushEnvelope is the body Pub/Sub POSTs to a push endpoint.
type PushEnvelope struct {
	Message struct {
		Data       []byte            `json:"data"` // base64 in JSON; decoded by encoding/json
		Attributes map[string]string `json:"attributes"`
		MessageID  string            `json:"messageId"`
	} `json:"message"`
	Subscription string `json:"subscription"`
	// Set only when the subscription has a dead-letter policy. Starts at 1.
	DeliveryAttempt int `json:"deliveryAttempt"`
}

// DecodePush reads a push request body into a Delivery.
func DecodePush(r *http.Request) (Delivery, error) {
	var env PushEnvelope
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		return Delivery{}, err
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Delivery{}, fmt.Errorf("decode push envelope: %w", err)
	}
	if env.DeliveryAttempt == 0 {
		env.DeliveryAttempt = 1
	}
	return Delivery{
		Data:         env.Message.Data,
		Attributes:   env.Message.Attributes,
		MessageID:    env.Message.MessageID,
		Attempt:      env.DeliveryAttempt,
		Subscription: env.Subscription,
	}, nil
}
