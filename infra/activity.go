package main

import (
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/bigquery"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/firebaserules"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/firestore"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

//go:embed firestore.rules.tmpl
var firestoreRulesTemplate string

// setupActivity builds the message-activity pipeline behind the admin
// Activity page:
//
//	API, worker ──publish──▶ topic "activity" ──┬─▶ worker (push / pull) ──▶ Firestore "activity" ──▶ browser (real time)
//	                                            └─▶ BigQuery analytics.message_activity (history)
//
// The platform-specific delivery to the worker (push on Cloud Run, pull on
// GKE) is created in cloudrun.go / gke.go from s.activityTopic.
func (s *shared) setupActivity(ctx *pulumi.Context) error {
	afterAPIs := pulumi.DependsOn(s.apis)

	topic, err := pubsub.NewTopic(ctx, "activity", &pubsub.TopicArgs{
		Name:                     pulumi.String("activity"),
		MessageRetentionDuration: pulumi.String("86400s"),
	}, afterAPIs)
	if err != nil {
		return err
	}
	s.activityTopic = topic
	for name, member := range map[string]pulumi.StringInput{
		"api-publishes-activity":    s.apiMember,
		"worker-publishes-activity": s.workerMember,
	} {
		if _, err := pubsub.NewTopicIAMMember(ctx, name, &pubsub.TopicIAMMemberArgs{
			Topic: topic.Name, Role: pulumi.String("roles/pubsub.publisher"), Member: member,
		}); err != nil {
			return err
		}
	}

	// ---- History: Pub/Sub writes every event straight into BigQuery ----
	// Columns match messages.Activity in the app.
	table, err := bigquery.NewTable(ctx, "message-activity", &bigquery.TableArgs{
		DatasetId:          s.dataset.DatasetId,
		TableId:            pulumi.String("message_activity"),
		DeletionProtection: pulumi.Bool(s.deletionProtection),
		TimePartitioning: &bigquery.TableTimePartitioningArgs{
			Type:  pulumi.String("DAY"),
			Field: pulumi.String("at"),
		},
		Clusterings: pulumi.StringArray{pulumi.String("topic"), pulumi.String("kind")},
		Schema: pulumi.String(`[
  {"name": "at",           "type": "TIMESTAMP", "mode": "REQUIRED"},
  {"name": "kind",         "type": "STRING",    "mode": "REQUIRED", "description": "published, received, acked, retrying, nacked, dead_lettered"},
  {"name": "topic",        "type": "STRING",    "mode": "REQUIRED"},
  {"name": "service",      "type": "STRING",    "mode": "REQUIRED"},
  {"name": "subscription", "type": "STRING",    "mode": "NULLABLE"},
  {"name": "message_id",   "type": "STRING",    "mode": "NULLABLE"},
  {"name": "outbox_id",    "type": "STRING",    "mode": "NULLABLE"},
  {"name": "attempt",      "type": "INT64",     "mode": "NULLABLE"},
  {"name": "summary",      "type": "STRING",    "mode": "NULLABLE"},
  {"name": "report_id",    "type": "STRING",    "mode": "NULLABLE"},
  {"name": "duration_ms",  "type": "INT64",     "mode": "NULLABLE"},
  {"name": "error",        "type": "STRING",    "mode": "NULLABLE"}
]`),
	})
	if err != nil {
		return err
	}
	historySub, err := pubsub.NewSubscription(ctx, "activity-to-bigquery", &pubsub.SubscriptionArgs{
		Name:             pulumi.String("activity-to-bigquery"),
		Topic:            topic.Name,
		ExpirationPolicy: &pubsub.SubscriptionExpirationPolicyArgs{Ttl: pulumi.String("")},
		BigqueryConfig: &pubsub.SubscriptionBigqueryConfigArgs{
			Table:             pulumi.Sprintf("%s.%s.%s", s.project, s.dataset.DatasetId, table.TableId),
			UseTableSchema:    pulumi.Bool(true),
			DropUnknownFields: pulumi.Bool(true),
		},
		DeadLetterPolicy: s.deadLetterPolicy(emailsMaxAttempts),
	}, pulumi.DependsOn(s.bqGrants))
	if err != nil {
		return err
	}
	if err := s.allowDeadLettering(ctx, "activity-to-bigquery", historySub); err != nil {
		return err
	}

	// ---- Live feed: Firestore ------------------------------------------
	deletion := "ABANDON"
	protection := "DELETE_PROTECTION_ENABLED"
	if !s.deletionProtection {
		deletion, protection = "DELETE", "DELETE_PROTECTION_DISABLED"
	}
	db, err := firestore.NewDatabase(ctx, "firestore", &firestore.DatabaseArgs{
		Project:               pulumi.String(s.project),
		Name:                  pulumi.String("(default)"),
		LocationId:            pulumi.String(s.region),
		Type:                  pulumi.String("FIRESTORE_NATIVE"),
		DeletionPolicy:        pulumi.String(deletion),
		DeleteProtectionState: pulumi.String(protection),
	}, afterAPIs)
	if err != nil {
		return err
	}
	s.firestoreDB = db

	// Firestore deletes events once expire_at passes (usually within a day).
	if _, err := firestore.NewField(ctx, "activity-ttl", &firestore.FieldArgs{
		Project:    pulumi.String(s.project),
		Database:   db.Name,
		Collection: pulumi.String("activity"),
		Field:      pulumi.String("expire_at"),
		TtlConfig:  &firestore.FieldTtlConfigArgs{},
	}); err != nil {
		return err
	}

	// Live plays for the Live data page (written by the worker from the
	// events-to-firestore subscription): 2-day TTL, and an index for "this
	// sport's latest plays".
	if _, err := firestore.NewField(ctx, "plays-ttl", &firestore.FieldArgs{
		Project:    pulumi.String(s.project),
		Database:   db.Name,
		Collection: pulumi.String("plays"),
		Field:      pulumi.String("expire_at"),
		TtlConfig:  &firestore.FieldTtlConfigArgs{},
	}); err != nil {
		return err
	}
	if _, err := firestore.NewIndex(ctx, "plays-by-league", &firestore.IndexArgs{
		Project:    pulumi.String(s.project),
		Database:   db.Name,
		Collection: pulumi.String("plays"),
		QueryScope: pulumi.String("COLLECTION"),
		Fields: firestore.IndexFieldArray{
			&firestore.IndexFieldArgs{FieldPath: pulumi.String("league"), Order: pulumi.String("ASCENDING")},
			&firestore.IndexFieldArgs{FieldPath: pulumi.String("at"), Order: pulumi.String("DESCENDING")},
		},
	}); err != nil {
		return err
	}

	// The worker writes the feed with its service account.
	if _, err := projects.NewIAMMember(ctx, "worker-firestore", &projects.IAMMemberArgs{
		Project: pulumi.String(s.project),
		Role:    pulumi.String("roles/datastore.user"),
		Member:  s.workerMember,
	}); err != nil {
		return err
	}

	// Security rules: only admins read; no browser writes.
	ruleset, err := firebaserules.NewRuleset(ctx, "firestore-rules", &firebaserules.RulesetArgs{
		Project: pulumi.String(s.project),
		Source: &firebaserules.RulesetSourceArgs{
			Files: firebaserules.RulesetSourceFileArray{
				&firebaserules.RulesetSourceFileArgs{
					Name:    pulumi.String("firestore.rules"),
					Content: pulumi.String(firestoreRules(s.adminEmails)),
				},
			},
		},
	}, pulumi.Provider(s.fb.provider), pulumi.DependsOn([]pulumi.Resource{db}))
	if err != nil {
		return err
	}
	// The release has a fixed name, so a rules change must delete the old
	// release before creating the new one (a few seconds without a
	// release, during which Firestore denies all browser reads).
	_, err = firebaserules.NewRelease(ctx, "firestore-rules", &firebaserules.ReleaseArgs{
		Project:     pulumi.String(s.project),
		Name:        pulumi.String("cloud.firestore"),
		RulesetName: pulumi.Sprintf("projects/%s/rulesets/%s", s.project, ruleset.Name),
	}, pulumi.Provider(s.fb.provider), pulumi.DeleteBeforeReplace(true))
	return err
}

// firestoreRules fills the admin email list into the rules template, so the
// Activity feed follows the same admin definition as the API (custom claim,
// or a verified email in adminEmails).
func firestoreRules(adminEmails string) string {
	emails := []string{}
	for _, e := range strings.Split(adminEmails, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			emails = append(emails, e)
		}
	}
	list, _ := json.Marshal(emails) // a JSON string array is a valid rules list literal
	return strings.ReplaceAll(firestoreRulesTemplate, "__ADMIN_EMAILS__", string(list))
}
