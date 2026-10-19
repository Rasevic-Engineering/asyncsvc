package main

import (
	"strconv"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/firestore"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// setupIngester creates what the live sports ingester needs, on any
// platform: its identity, the ingest-commands topic (admins → ingester) with
// a pull subscription, permissions, and a TTL on its Firestore status docs.
//
//	admin UI ──POST /v1/admin/replays──▶ API ──▶ topic "ingest-commands" ──pull──▶ ingester
//	NHL API ◀──poll── ingester ──▶ topic "events" ──▶ BigQuery
//	ingester ──status──▶ Firestore "ingest" ──▶ admin Live data page
//
// The ingester process itself runs as a Cloud Run worker pool (cloudrun.go)
// or a Deployment (gke.go).
func (s *shared) setupIngester(ctx *pulumi.Context) error {
	afterAPIs := pulumi.DependsOn(s.apis)

	sa, err := newSA(ctx, "ingester", "Live sports data ingester", afterAPIs)
	if err != nil {
		return err
	}
	s.ingesterSA = sa
	member := saMember(sa)

	topic, err := pubsub.NewTopic(ctx, "ingest-commands", &pubsub.TopicArgs{
		Name:                     pulumi.String("ingest-commands"),
		MessageRetentionDuration: pulumi.String("86400s"),
	}, afterAPIs)
	if err != nil {
		return err
	}
	s.ingestCommandsTopic = topic

	sub, err := pubsub.NewSubscription(ctx, "ingest-commands-pull", &pubsub.SubscriptionArgs{
		Name:               pulumi.String("ingest-commands-pull"),
		Topic:              topic.Name,
		AckDeadlineSeconds: pulumi.Int(60),
		ExpirationPolicy:   &pubsub.SubscriptionExpirationPolicyArgs{Ttl: pulumi.String("")},
		RetryPolicy:        retryPolicy(),
		DeadLetterPolicy:   s.deadLetterPolicy(emailsMaxAttempts),
	})
	if err != nil {
		return err
	}
	if err := s.allowDeadLettering(ctx, "ingest-commands-pull", sub); err != nil {
		return err
	}
	if _, err := pubsub.NewSubscriptionIAMMember(ctx, "ingester-subscribes-commands", &pubsub.SubscriptionIAMMemberArgs{
		Subscription: sub.Name, Role: pulumi.String("roles/pubsub.subscriber"), Member: member,
	}); err != nil {
		return err
	}

	for name, b := range map[string]struct {
		topic  *pubsub.Topic
		member pulumi.StringInput
	}{
		"ingester-publishes-events":   {s.eventsTopic, member},
		"ingester-publishes-activity": {s.activityTopic, member},
		"api-publishes-ingest-cmds":   {topic, s.apiMember},
	} {
		if _, err := pubsub.NewTopicIAMMember(ctx, name, &pubsub.TopicIAMMemberArgs{
			Topic: b.topic.Name, Role: pulumi.String("roles/pubsub.publisher"), Member: b.member,
		}); err != nil {
			return err
		}
	}

	// Status documents in Firestore ("ingest" collection).
	if _, err := projects.NewIAMMember(ctx, "ingester-firestore", &projects.IAMMemberArgs{
		Project: pulumi.String(s.project),
		Role:    pulumi.String("roles/datastore.user"),
		Member:  member,
	}); err != nil {
		return err
	}
	_, err = firestore.NewField(ctx, "ingest-ttl", &firestore.FieldArgs{
		Project:    pulumi.String(s.project),
		Database:   s.firestoreDB.Name,
		Collection: pulumi.String("ingest"),
		Field:      pulumi.String("expire_at"),
		TtlConfig:  &firestore.FieldTtlConfigArgs{},
	})
	return err
}

// ingesterEnv is the ingester's configuration, shared by both platforms.
func (s *shared) ingesterEnv() []envVar {
	return []envVar{
		{"GCP_PROJECT", pulumi.String(s.project)},
		{"TOPIC_EVENTS", s.eventsTopic.Name},
		{"TOPIC_ACTIVITY", s.activityTopic.Name},
		{"SUB_INGEST_COMMANDS", pulumi.String("ingest-commands-pull")},
		{"NHL_ENABLED", pulumi.String(strconv.FormatBool(s.ingesterNHL))},
		{"IMAGE", s.image},
	}
}

func (s *shared) ingesterInstances() int {
	if s.ingesterEnabled {
		return 1
	}
	return 0
}
