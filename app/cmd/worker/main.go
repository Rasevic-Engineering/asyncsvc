// Command worker processes Pub/Sub messages.
//
//	WORKER_MODE=push  (Cloud Run) Pub/Sub POSTs to /pubsub/*; Cloud Scheduler
//	                  POSTs to /tasks/relay-outbox.
//	WORKER_MODE=pull  (GKE) streaming pull from SUB_REPORTS and SUB_EMAILS, and
//	                  an in-process outbox sweep. HTTP serves health checks.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/example/asyncsvc/internal/blob"
	"github.com/example/asyncsvc/internal/config"
	"github.com/example/asyncsvc/internal/db"
	"github.com/example/asyncsvc/internal/httpx"
	"github.com/example/asyncsvc/internal/mail"
	"github.com/example/asyncsvc/internal/outbox"
	"github.com/example/asyncsvc/internal/pubsubx"
	"github.com/example/asyncsvc/internal/reports"
	"github.com/example/asyncsvc/internal/worker"
)

func main() {
	slog.SetDefault(httpx.NewLogger())
	if err := run(); err != nil {
		slog.Error("worker exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBInstance)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	pub, err := pubsubx.NewPublisher(ctx, cfg.Project)
	if err != nil {
		return err
	}
	defer pub.Close()

	store, err := blob.New(ctx, cfg.ReportsBucket, cfg.ReportsDir)
	if err != nil {
		return err
	}

	var source reports.Source
	switch cfg.ReportSource {
	case "bigquery":
		bq, err := reports.NewBigQuerySource(ctx, cfg.Project, cfg.BQDataset, cfg.BQEventsTable, cfg.BQLocation)
		if err != nil {
			return err
		}
		defer bq.Close()
		source = bq
	case "fake":
		source = reports.FakeSource{Delay: 3 * time.Second}
	default:
		return fmt.Errorf("unknown REPORT_SOURCE %q", cfg.ReportSource)
	}

	var mailer mail.Mailer = mail.LogMailer{}
	if cfg.MailProvider == "sendgrid" {
		mailer = &mail.SendGrid{APIKey: cfg.SendGridAPIKey, From: cfg.MailFrom}
	}

	wk := &worker.Worker{
		Pool:            pool,
		Reports:         &reports.Repo{Pool: pool},
		Source:          source,
		Store:           store,
		Mailer:          mailer,
		Relay:           &outbox.Relay{Pool: pool, Pub: pub},
		TopicEmails:     cfg.TopicEmails,
		PublicAPIURL:    cfg.PublicAPIURL,
		MaxAttempts:     cfg.ReportsMaxAttempts,
		GenerateTimeout: cfg.ReportTimeout,
		StaleAfter:      cfg.ReportStaleAfter,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpx.Serve(gctx, ":"+cfg.Port, wk.Routes()) })

	if cfg.WorkerMode == "pull" {
		// Keep extending ack deadlines a bit past the report timeout so a
		// running report is never redelivered while it's still in progress.
		maxExt := cfg.ReportTimeout + 5*time.Minute
		for sub, h := range map[string]pubsubx.Handler{
			cfg.SubReports: wk.Handle(wk.HandleReport),
			cfg.SubEmails:  wk.Handle(wk.HandleEmail),
		} {
			g.Go(func() error {
				slog.Info("consuming", "subscription", sub)
				if err := pub.Consume(gctx, sub, cfg.PullMaxOutstanding, maxExt, h); err != nil {
					return fmt.Errorf("consume %s: %w", sub, err)
				}
				return nil
			})
		}
	}
	if cfg.OutboxSweepInterval > 0 {
		g.Go(func() error { wk.SweepOutbox(gctx, cfg.OutboxSweepInterval); return nil })
	}
	return g.Wait()
}
