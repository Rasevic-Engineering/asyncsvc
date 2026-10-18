// Package messages defines the Pub/Sub message payloads shared by API and worker.
package messages

import "github.com/google/uuid"

// ReportRequested is published to the report-requests topic.
type ReportRequested struct {
	ReportID uuid.UUID `json:"report_id"`
}

// EmailRequested is published to the email-requests topic.
type EmailRequested struct {
	Template string            `json:"template"`
	To       string            `json:"to"`
	Data     map[string]string `json:"data"`
}

// Event is published to the events topic, which a BigQuery subscription writes
// straight into the analytics.events table. Field names and types must match
// the table schema (TIMESTAMP columns are microseconds since the Unix epoch).
type Event struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	UserID     string `json:"user_id"`
	OccurredAt int64  `json:"occurred_at"`
	ReceivedAt int64  `json:"received_at"`
	Properties string `json:"properties,omitempty"` // JSON-encoded object
}

// AttrOutboxID is the message attribute carrying the outbox row id, which
// consumers use as an idempotency key.
const AttrOutboxID = "outbox_id"

// Activity is a lifecycle event about a message on another topic, published
// to the activity topic. Field names and types match the BigQuery table
// analytics.message_activity (at is microseconds since the Unix epoch).
type Activity struct {
	At           int64  `json:"at"`
	Kind         string `json:"kind"`
	Topic        string `json:"topic"`
	Subscription string `json:"subscription,omitempty"`
	MessageID    string `json:"message_id,omitempty"`
	OutboxID     string `json:"outbox_id,omitempty"`
	Attempt      int64  `json:"attempt,omitempty"`
	Summary      string `json:"summary,omitempty"`
	ReportID     string `json:"report_id,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	Error        string `json:"error,omitempty"`
	Service      string `json:"service"`
}

// Activity kinds.
const (
	ActivityPublished    = "published"
	ActivityReceived     = "received"
	ActivityAcked        = "acked"
	ActivityRetrying     = "retrying"
	ActivityNacked       = "nacked"
	ActivityDeadLettered = "dead_lettered"
)
