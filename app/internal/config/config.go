// Package config loads service configuration from environment variables.
// The same image runs as the API and the worker; each reads what it needs.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port    string
	Project string

	DatabaseURL string
	// Cloud SQL instance connection name (project:region:instance). When set,
	// connections go through the Cloud SQL Go connector (used on GKE).
	DBInstance string

	// Worker delivery mode: "push" (Cloud Run: Pub/Sub POSTs to us) or
	// "pull" (GKE: we stream-pull from subscriptions).
	WorkerMode         string
	SubReports         string
	SubEmails          string
	PullMaxOutstanding int
	// In-process outbox sweep interval; 0 disables (Cloud Run uses Cloud
	// Scheduler instead, since idle instances get no CPU).
	OutboxSweepInterval time.Duration

	// Report generation timeout and the age after which a "running" report
	// is assumed abandoned. Push mode is bounded by the 600s ack deadline;
	// pull mode extends the deadline automatically, so it can run longer.
	ReportTimeout    time.Duration
	ReportStaleAfter time.Duration

	TopicReports string
	TopicEmails  string
	TopicEvents  string
	// Message lifecycle events for the admin Activity page; empty disables.
	TopicActivity string
	SubActivity   string
	SubPlays      string // pull mode: live sports plays for the UI
	// Admin commands for the ingester (StatsBomb replays).
	TopicIngestCommands string
	// Must match the emails subscription's max delivery attempts.
	EmailMaxAttempts int

	// Report output: GCS bucket in the cloud, local directory for development.
	ReportsBucket string
	ReportsDir    string

	// Report data source: "bigquery" or "fake" (synthetic rows, for local dev).
	ReportSource  string
	BQDataset     string
	BQEventsTable string
	BQLocation    string

	// Must match the push subscription's max delivery attempts.
	ReportsMaxAttempts int

	MailProvider   string // "log" or "sendgrid"
	MailFrom       string
	SendGridAPIKey string

	// Directory with the built React app. Empty disables serving it.
	WebDir string

	// Firebase Auth. FirebaseProjectID is the token audience (normally the
	// GCP project). The Web* values are public and sent to the browser.
	FirebaseProjectID    string
	AdminEmails          []string
	TrustUnverifiedEmail bool // local emulator only
	WebFirebaseAPIKey    string
	WebFirebaseDomain    string
	WebAuthEmulatorURL   string
	WebGoogleSignIn      bool
	// Browser-facing Firestore emulator address (local development only).
	WebFirestoreEmulatorHost string
	// <project>.firebaseapp.com to proxy /__/auth/ to, when the web app uses
	// this service's own domain as its Firebase authDomain.
	FirebaseHandlerHost string

	// Base URL of the API, used in links inside emails.
	PublicAPIURL string
}

func Load() (Config, error) {
	c := Config{
		Port:                     env("PORT", "8080"),
		Project:                  env("GCP_PROJECT", "local"),
		TopicReports:             env("TOPIC_REPORTS", "report-requests"),
		TopicEmails:              env("TOPIC_EMAILS", "email-requests"),
		TopicEvents:              env("TOPIC_EVENTS", "events"),
		TopicActivity:            env("TOPIC_ACTIVITY", "activity"),
		SubActivity:              env("SUB_ACTIVITY", "activity-pull"),
		SubPlays:                 env("SUB_PLAYS", "plays-pull"),
		TopicIngestCommands:      env("TOPIC_INGEST_COMMANDS", "ingest-commands"),
		EmailMaxAttempts:         5,
		ReportsBucket:            os.Getenv("REPORTS_BUCKET"),
		ReportsDir:               env("REPORTS_DIR", "/tmp/reports"),
		ReportSource:             env("REPORT_SOURCE", "bigquery"),
		BQDataset:                env("BQ_DATASET", "analytics"),
		BQEventsTable:            env("BQ_EVENTS_TABLE", "events"),
		BQLocation:               env("BQ_LOCATION", "US"),
		MailProvider:             env("MAIL_PROVIDER", "log"),
		MailFrom:                 env("MAIL_FROM", "reports@example.com"),
		SendGridAPIKey:           os.Getenv("SENDGRID_API_KEY"),
		WebDir:                   os.Getenv("WEB_DIR"),
		WebFirebaseAPIKey:        os.Getenv("WEB_FIREBASE_API_KEY"),
		WebFirebaseDomain:        os.Getenv("WEB_FIREBASE_AUTH_DOMAIN"),
		WebAuthEmulatorURL:       os.Getenv("WEB_AUTH_EMULATOR_URL"),
		WebFirestoreEmulatorHost: os.Getenv("WEB_FIRESTORE_EMULATOR_HOST"),
		FirebaseHandlerHost:      os.Getenv("FIREBASE_AUTH_HANDLER_HOST"),
		PublicAPIURL:             strings.TrimRight(env("PUBLIC_API_URL", "http://localhost:8080"), "/"),
		ReportsMaxAttempts:       10,
		DBInstance:               os.Getenv("DB_INSTANCE_CONNECTION_NAME"),
		WorkerMode:               env("WORKER_MODE", "push"),
		SubReports:               env("SUB_REPORTS", "reports-pull"),
		SubEmails:                env("SUB_EMAILS", "emails-pull"),
		PullMaxOutstanding:       8,
	}

	var err error
	if c.OutboxSweepInterval, err = duration("OUTBOX_SWEEP_INTERVAL", 0); err != nil {
		return c, err
	}
	if c.ReportTimeout, err = duration("REPORT_TIMEOUT", 9*time.Minute); err != nil {
		return c, err
	}
	if c.ReportStaleAfter, err = duration("REPORT_STALE_AFTER", 15*time.Minute); err != nil {
		return c, err
	}
	if c.ReportStaleAfter <= c.ReportTimeout {
		return c, fmt.Errorf("REPORT_STALE_AFTER (%s) must exceed REPORT_TIMEOUT (%s)", c.ReportStaleAfter, c.ReportTimeout)
	}
	if c.WorkerMode != "push" && c.WorkerMode != "pull" {
		return c, fmt.Errorf("WORKER_MODE must be push or pull, got %q", c.WorkerMode)
	}
	if v := os.Getenv("PULL_MAX_OUTSTANDING"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid PULL_MAX_OUTSTANDING %q", v)
		}
		c.PullMaxOutstanding = n
	}

	if v := os.Getenv("EMAIL_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid EMAIL_MAX_ATTEMPTS %q", v)
		}
		c.EmailMaxAttempts = n
	}
	if v := os.Getenv("REPORTS_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("invalid REPORTS_MAX_ATTEMPTS %q", v)
		}
		c.ReportsMaxAttempts = n
	}

	c.FirebaseProjectID = env("FIREBASE_PROJECT_ID", c.Project)
	for _, e := range strings.Split(os.Getenv("ADMIN_EMAILS"), ",") {
		if e = strings.TrimSpace(e); e != "" {
			c.AdminEmails = append(c.AdminEmails, e)
		}
	}
	if c.TrustUnverifiedEmail, err = boolEnv("AUTH_TRUST_UNVERIFIED_EMAIL"); err != nil {
		return c, err
	}
	if c.TrustUnverifiedEmail && os.Getenv("FIREBASE_AUTH_EMULATOR_HOST") == "" {
		return c, fmt.Errorf("AUTH_TRUST_UNVERIFIED_EMAIL is only allowed with the Firebase Auth emulator")
	}
	if c.WebGoogleSignIn, err = boolEnv("WEB_GOOGLE_SIGN_IN"); err != nil {
		return c, err
	}

	dsn, err := databaseURL(c.DBInstance != "")
	if err != nil {
		return c, err
	}
	c.DatabaseURL = dsn

	if c.MailProvider == "sendgrid" && c.SendGridAPIKey == "" {
		return c, fmt.Errorf("MAIL_PROVIDER=sendgrid requires SENDGRID_API_KEY")
	}
	return c, nil
}

// databaseURL returns DATABASE_URL, or builds a keyword/value DSN from DB_* vars.
// On Cloud Run DB_HOST is the Cloud SQL unix socket dir: /cloudsql/<connection-name>.
// With the Cloud SQL connector the host is ignored (the connector dials).
func databaseURL(viaConnector bool) (string, error) {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u, nil
	}
	host, user, pass, name := os.Getenv("DB_HOST"), os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD"), os.Getenv("DB_NAME")
	if host == "" && viaConnector {
		host = "localhost"
	}
	if host == "" || user == "" || name == "" {
		return "", fmt.Errorf("set DATABASE_URL or DB_HOST, DB_USER, DB_PASSWORD and DB_NAME")
	}
	return fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable",
		quote(host), quote(user), quote(pass), quote(name)), nil
}

func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

func duration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid %s %q", key, v)
	}
	return d, nil
}

func boolEnv(key string) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid %s %q", key, v)
	}
	return b, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
