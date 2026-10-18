// Package mail renders templated emails and sends them through a provider.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"text/template"
	"time"
)

var (
	// ErrPermanent means retrying will not help (bad recipient, bad template).
	ErrPermanent = errors.New("permanent mail failure")
	// ErrRateLimited means the provider asked us to slow down.
	ErrRateLimited = errors.New("mail provider rate limited")
)

type Message struct {
	To      string
	Subject string
	Text    string
}

type Mailer interface {
	Send(ctx context.Context, m Message) error
}

type tmpl struct {
	subject, body *template.Template
}

func mustTmpl(subject, body string) tmpl {
	return tmpl{
		subject: template.Must(template.New("s").Option("missingkey=error").Parse(subject)),
		body:    template.Must(template.New("b").Option("missingkey=error").Parse(body)),
	}
}

var templates = map[string]tmpl{
	"report_ready": mustTmpl(
		`Your {{.kind}} report is ready`,
		`Hi,

Your {{.kind}} report for {{.from}} to {{.to}} is ready ({{.rows}} rows).

Get a download link here (links expire after 15 minutes):
{{.url}}
`),
	"report_failed": mustTmpl(
		`Your {{.kind}} report failed`,
		`Hi,

Unfortunately your {{.kind}} report for {{.from}} to {{.to}} could not be generated.

Reason: {{.error}}
`),
}

func Render(name, to string, data map[string]string) (Message, error) {
	t, ok := templates[name]
	if !ok {
		return Message{}, fmt.Errorf("%w: unknown template %q", ErrPermanent, name)
	}
	var subj, body strings.Builder
	if err := t.subject.Execute(&subj, data); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	if err := t.body.Execute(&body, data); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	return Message{To: to, Subject: subj.String(), Text: body.String()}, nil
}

// LogMailer just logs emails. Used for local development.
type LogMailer struct{}

func (LogMailer) Send(ctx context.Context, m Message) error {
	slog.InfoContext(ctx, "email (log mailer)", "to", m.To, "subject", m.Subject, "body", m.Text)
	return nil
}

// SendGrid sends through the SendGrid v3 HTTP API.
type SendGrid struct {
	APIKey string
	From   string
	HTTP   *http.Client
}

func (s *SendGrid) Send(ctx context.Context, m Message) error {
	body, _ := json.Marshal(map[string]any{
		"personalizations": []any{map[string]any{"to": []any{map[string]string{"email": m.To}}}},
		"from":             map[string]string{"email": s.From},
		"subject":          m.Subject,
		"content":          []any{map[string]string{"type": "text/plain", "value": m.Text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.sendgrid.com/v3/mail/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")

	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden:
		return fmt.Errorf("%w: sendgrid %d: %s", ErrPermanent, resp.StatusCode, msg)
	default:
		return fmt.Errorf("sendgrid %d: %s", resp.StatusCode, msg)
	}
}
