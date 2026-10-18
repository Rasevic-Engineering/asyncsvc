// Command devsetup creates topics and push subscriptions on the Pub/Sub
// emulator, mirroring what Pulumi creates in GCP. Local development only.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func main() {
	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		log.Fatal("PUBSUB_EMULATOR_HOST must be set; refusing to touch real Pub/Sub")
	}
	project := envOr("GCP_PROJECT", "local")
	workerURL := envOr("WORKER_URL", "http://worker:8081")
	mode := envOr("WORKER_MODE", "push") // push mirrors Cloud Run, pull mirrors GKE

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := pubsub.NewClient(ctx, project)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	topic := func(id string) string { return fmt.Sprintf("projects/%s/topics/%s", project, id) }
	sub := func(id string) string { return fmt.Sprintf("projects/%s/subscriptions/%s", project, id) }

	for _, t := range []string{"report-requests", "email-requests", "events", "dead-letter"} {
		_, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic(t)})
		check(err, "topic "+t)
	}

	retry := &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(2 * time.Second), MaximumBackoff: durationpb.New(30 * time.Second)}
	// Only one mode's subscriptions are created: with both, every message
	// would be processed twice (once per subscription).
	var subs []*pubsubpb.Subscription
	switch mode {
	case "push":
		subs = []*pubsubpb.Subscription{
			{
				Name: sub("reports-push"), Topic: topic("report-requests"), AckDeadlineSeconds: 600, RetryPolicy: retry,
				PushConfig: &pubsubpb.PushConfig{PushEndpoint: workerURL + "/pubsub/reports"},
			},
			{
				Name: sub("emails-push"), Topic: topic("email-requests"), AckDeadlineSeconds: 60, RetryPolicy: retry,
				PushConfig: &pubsubpb.PushConfig{PushEndpoint: workerURL + "/pubsub/emails"},
			},
		}
	case "pull":
		subs = []*pubsubpb.Subscription{
			{Name: sub("reports-pull"), Topic: topic("report-requests"), AckDeadlineSeconds: 60, RetryPolicy: retry},
			{Name: sub("emails-pull"), Topic: topic("email-requests"), AckDeadlineSeconds: 60, RetryPolicy: retry},
		}
	default:
		log.Fatalf("WORKER_MODE must be push or pull, got %q", mode)
	}
	// Stand-in for the BigQuery subscription so local events are inspectable.
	subs = append(subs, &pubsubpb.Subscription{Name: sub("events-debug"), Topic: topic("events")})
	for _, s := range subs {
		_, err := c.SubscriptionAdminClient.CreateSubscription(ctx, s)
		check(err, "subscription "+s.Name)
	}
	log.Printf("emulator topics and %s subscriptions ready", mode)
}

func check(err error, what string) {
	if err == nil {
		log.Printf("created %s", what)
		return
	}
	if status.Code(err) == codes.AlreadyExists {
		log.Printf("exists  %s", what)
		return
	}
	log.Fatalf("create %s: %v", what, err)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
