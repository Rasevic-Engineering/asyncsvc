CREATE TABLE reports (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL,
    params       jsonb       NOT NULL,
    notify_email text,
    status       text        NOT NULL DEFAULT 'pending', -- pending | running | ready | failed
    file_key     text,
    row_count    bigint,
    error        text,
    attempts     int         NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    completed_at timestamptz
);

-- Transactional outbox: rows are written in the same transaction as the
-- business change, then published to Pub/Sub by outbox.Relay.
CREATE TABLE outbox (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    topic        text        NOT NULL,
    payload      jsonb       NOT NULL,
    attributes   jsonb       NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    attempts     int         NOT NULL DEFAULT 0,
    last_error   text
);

CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;

-- Dedupe for at-least-once Pub/Sub delivery of email messages.
CREATE TABLE email_deliveries (
    message_id uuid PRIMARY KEY, -- outbox id of the email request
    recipient  text        NOT NULL,
    template   text        NOT NULL,
    status     text        NOT NULL DEFAULT 'sending', -- sending | sent
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz
);
