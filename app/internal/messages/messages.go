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
