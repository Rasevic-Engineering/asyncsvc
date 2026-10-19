// Command api serves the public REST API.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/example/asyncsvc/internal/activity"
	"github.com/example/asyncsvc/internal/api"
	"github.com/example/asyncsvc/internal/auth"
	"github.com/example/asyncsvc/internal/blob"
	"github.com/example/asyncsvc/internal/config"
	"github.com/example/asyncsvc/internal/db"
	"github.com/example/asyncsvc/internal/httpx"
	"github.com/example/asyncsvc/internal/outbox"
	"github.com/example/asyncsvc/internal/pubsubx"
	"github.com/example/asyncsvc/internal/reports"
	"github.com/example/asyncsvc/internal/version"
)

func main() {
	slog.SetDefault(httpx.NewLogger())
	if err := run(); err != nil {
		slog.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	v := version.Get()
	slog.Info("starting", "commit", v.Commit, "image", v.Image)

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

	verifier, err := auth.NewFirebase(ctx, cfg.FirebaseProjectID, cfg.AdminEmails, cfg.TrustUnverifiedEmail)
	if err != nil {
		return err
	}
	var web api.WebConfig
	web.Firebase.APIKey = cfg.WebFirebaseAPIKey
	web.Firebase.AuthDomain = cfg.WebFirebaseDomain
	web.Firebase.ProjectID = cfg.FirebaseProjectID
	web.AuthEmulatorURL = cfg.WebAuthEmulatorURL
	web.GoogleSignIn = cfg.WebGoogleSignIn
	web.FirestoreEmulatorHost = cfg.WebFirestoreEmulatorHost

	rec := &activity.Recorder{Pub: pub, Topic: cfg.TopicActivity, Service: "api"}
	srv := &api.Server{
		Pool:                pool,
		Reports:             &reports.Repo{Pool: pool},
		Relay:               &outbox.Relay{Pool: pool, Pub: pub, Activity: rec},
		Activity:            rec,
		Pub:                 pub,
		Store:               store,
		TopicReports:        cfg.TopicReports,
		TopicEvents:         cfg.TopicEvents,
		TopicIngestCommands: cfg.TopicIngestCommands,
		WebDir:              cfg.WebDir,
		Auth:                verifier,
		Web:                 web,
		FirebaseHandlerHost: cfg.FirebaseHandlerHost,
	}
	return httpx.Serve(ctx, ":"+cfg.Port, srv.Routes())
}
