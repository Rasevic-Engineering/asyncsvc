// Command ingester feeds live sports data into the events pipeline:
// play-by-play for live NHL, MLB, NBA, NFL and soccer games (INGEST_LEAGUES,
// default all), and StatsBomb soccer replays started by admins
// (ingest-commands topic). It runs continuously (a Cloud Run worker pool, or
// a Deployment on GKE) and serves /health and /version.
//
// It doesn't need the database: progress lives in Firestore.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/firestore"
	"golang.org/x/sync/errgroup"

	"github.com/example/asyncsvc/internal/activity"
	"github.com/example/asyncsvc/internal/httpx"
	"github.com/example/asyncsvc/internal/ingest"
	"github.com/example/asyncsvc/internal/pubsubx"
	"github.com/example/asyncsvc/internal/version"
)

func main() {
	slog.SetDefault(httpx.NewLogger())
	if err := run(); err != nil {
		slog.Error("ingester exited", "err", err)
		os.Exit(1)
	}
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	v := version.Get()
	slog.Info("starting", "commit", v.Commit, "image", v.Image)

	project := env("GCP_PROJECT", "local")
	fsProject := env("FIREBASE_PROJECT_ID", project)

	pub, err := pubsubx.NewPublisher(ctx, project)
	if err != nil {
		return err
	}
	defer pub.Close()

	fs, err := firestore.NewClient(ctx, fsProject) // honors FIRESTORE_EMULATOR_HOST
	if err != nil {
		return fmt.Errorf("firestore client: %w", err)
	}
	defer fs.Close()

	rec := &activity.Recorder{Pub: pub, Topic: env("TOPIC_ACTIVITY", "activity"), Service: "ingester"}
	in := ingest.New(pub, env("TOPIC_EVENTS", "events"), rec, &ingest.FirestoreStatus{Client: fs})
	leagues, err := ingest.Leagues(env("INGEST_LEAGUES", "nhl,mlb,nba,nfl,soccer"))
	if err != nil {
		return err
	}
	in.Tracker.Leagues = leagues
	in.Start(ctx)
	names := make([]string, len(leagues))
	for i, l := range leagues {
		names[i] = l.Name()
	}
	slog.Info("ingester running", "leagues", strings.Join(names, ","))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /health", httpx.Health)
		mux.HandleFunc("GET /healthz", httpx.Health)
		mux.HandleFunc("GET /version", version.Handler)
		return httpx.Serve(gctx, ":"+env("PORT", "8080"), mux)
	})
	g.Go(func() error {
		sub := env("SUB_INGEST_COMMANDS", "ingest-commands-pull")
		slog.Info("consuming", "subscription", sub)
		return pub.Consume(gctx, sub, 10, 2*time.Minute, in.HandleCommand)
	})
	err = g.Wait()
	in.Wait() // let replays save their position
	return err
}
