// Pulumi program that stands up the async service on GCP. The data plane is
// shared; the compute platform is selectable with `asyncsvc:platform`:
//
//	cloudrun (default)  Cloud Run services; Pub/Sub PUSHES to the worker;
//	                    Cloud Scheduler drives the outbox sweeper.
//	gke                 GKE Autopilot; the worker PULLS from Pub/Sub and
//	                    sweeps the outbox in-process; Workload Identity.
//
// Shared: Cloud SQL Postgres (state + outbox), Pub/Sub topics + dead-letter
// topic, BigQuery (events table fed by a BigQuery subscription), Cloud
// Storage (reports), Secret Manager, Artifact Registry, service accounts.
package main

import (
	"fmt"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/bigquery"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/sql"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

const (
	appName = "asyncsvc"

	// Passed to the worker as REPORTS_MAX_ATTEMPTS; must match the reports
	// subscription's dead-letter policy.
	reportsMaxAttempts = 10
	emailsMaxAttempts  = 5
)

var sharedAPIs = []string{
	"artifactregistry.googleapis.com",
	"bigquery.googleapis.com",
	"bigquerystorage.googleapis.com",
	"firebase.googleapis.com",
	"identitytoolkit.googleapis.com", // Firebase Auth / Identity Platform
	"iam.googleapis.com",
	"iamcredentials.googleapis.com", // signed URLs without key files
	"pubsub.googleapis.com",
	"secretmanager.googleapis.com",
	"sqladmin.googleapis.com",
	"storage.googleapis.com",
}

var platformAPIs = map[string][]string{
	"cloudrun": {"run.googleapis.com", "cloudscheduler.googleapis.com"},
	"gke":      {"container.googleapis.com"},
}

type settings struct {
	project, region    string
	platform           string
	dbTier             string
	bqLocation         string
	mailFrom           string
	publicAPI          bool
	deletionProtection bool
	sendgridKey        pulumi.StringOutput
	useSendGrid        bool
	workerReplicas     int // gke only
	image              string

	// Auth.
	adminEmails        string // comma-separated
	useGoogleSignIn    bool
	googleClientID     string
	googleClientSecret pulumi.StringOutput
}

// shared holds resources both platforms build on.
type shared struct {
	settings
	apis        []pulumi.Resource
	pubsubAgent *projects.ServiceIdentity

	apiSA, workerSA         *serviceaccount.Account
	apiMember, workerMember pulumi.StringOutput

	repo *artifactregistry.Repository
	// Container image to run, by digest (asyncsvc:image, set by `make image`).
	image pulumi.StringInput

	dbInstance     *sql.DatabaseInstance
	dbName, dbUser pulumi.StringOutput
	dbPassword     pulumi.StringOutput
	dbSecret       *secretmanager.Secret
	sendgridSecret *secretmanager.Secret // nil unless sendgridApiKey is set
	secretGrants   []pulumi.Resource

	bucket      *storage.Bucket
	dataset     *bigquery.Dataset
	eventsTable *bigquery.Table
	bqGrants    []pulumi.Resource

	reportsTopic, emailsTopic, eventsTopic, deadLetterTopic *pubsub.Topic

	fb *firebaseAuth
}

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		st, err := loadSettings(ctx)
		if err != nil {
			return err
		}
		s, err := buildShared(ctx, st)
		if err != nil {
			return err
		}
		if st.image == "" {
			// First deploy of a new project: create the data plane and the
			// registry, then push an image and run `pulumi up` again.
			ctx.Log.Warn("asyncsvc:image is not set, so the API and worker were not deployed. "+
				"Run `make image` to build and push the container (it sets asyncsvc:image), then `make deploy`.", nil)
			return nil
		}
		switch st.platform {
		case "cloudrun":
			return deployCloudRun(ctx, s)
		case "gke":
			return deployGKE(ctx, s)
		}
		return nil
	})
}

func loadSettings(ctx *pulumi.Context) (settings, error) {
	gcpCfg := config.New(ctx, "gcp")
	cfg := config.New(ctx, "")
	st := settings{
		project:    gcpCfg.Require("project"),
		region:     orDefault(gcpCfg.Get("region"), "us-central1"),
		platform:   orDefault(cfg.Get("platform"), "cloudrun"),
		dbTier:     orDefault(cfg.Get("dbTier"), "db-f1-micro"),
		bqLocation: orDefault(cfg.Get("bigqueryLocation"), "US"),
		mailFrom:   orDefault(cfg.Get("mailFrom"), "reports@example.com"),
		// The API authenticates every /v1 request with Firebase, and browsers
		// must reach the UI, so it is public at the network level by default.
		publicAPI:          true,
		adminEmails:        cfg.Get("adminEmails"),
		googleClientID:     cfg.Get("googleOAuthClientId"),
		deletionProtection: true,
		workerReplicas:     2,
		image:              cfg.Get("image"),
	}
	if _, ok := platformAPIs[st.platform]; !ok {
		return st, fmt.Errorf("asyncsvc:platform must be cloudrun or gke, got %q", st.platform)
	}
	if v, err := cfg.TryBool("publicApi"); err == nil {
		st.publicAPI = v
	}
	if st.googleClientID != "" {
		secret, err := cfg.TrySecret("googleOAuthClientSecret")
		if err != nil {
			return st, fmt.Errorf("googleOAuthClientId is set, so googleOAuthClientSecret (secret) is required")
		}
		st.googleClientSecret, st.useGoogleSignIn = secret, true
	}
	if v, err := cfg.TryBool("deletionProtection"); err == nil {
		st.deletionProtection = v
	}
	if v, err := cfg.TryInt("workerReplicas"); err == nil {
		st.workerReplicas = v
	}
	if key, err := cfg.TrySecret("sendgridApiKey"); err == nil {
		st.sendgridKey, st.useSendGrid = key, true
	}
	return st, nil
}

func buildShared(ctx *pulumi.Context, st settings) (*shared, error) {
	s := &shared{settings: st}

	// ---- APIs --------------------------------------------------------
	for _, api := range append(append([]string{}, sharedAPIs...), platformAPIs[st.platform]...) {
		svc, err := projects.NewService(ctx, api, &projects.ServiceArgs{
			Project:          pulumi.String(st.project),
			Service:          pulumi.String(api),
			DisableOnDestroy: pulumi.Bool(false),
		})
		if err != nil {
			return nil, err
		}
		s.apis = append(s.apis, svc)
	}
	afterAPIs := pulumi.DependsOn(s.apis)

	// Pub/Sub's Google-managed service agent. It needs roles to write dead
	// letters and to write into BigQuery (and, on Cloud Run, to mint push
	// OIDC tokens).
	var err error
	s.pubsubAgent, err = projects.NewServiceIdentity(ctx, "pubsub-agent", &projects.ServiceIdentityArgs{
		Project: pulumi.String(st.project),
		Service: pulumi.String("pubsub.googleapis.com"),
	}, afterAPIs)
	if err != nil {
		return nil, err
	}

	// ---- Service accounts (workload identities) -----------------------
	if s.apiSA, err = newSA(ctx, "api", "Async service API", afterAPIs); err != nil {
		return nil, err
	}
	if s.workerSA, err = newSA(ctx, "worker", "Async service worker", afterAPIs); err != nil {
		return nil, err
	}
	s.apiMember, s.workerMember = saMember(s.apiSA), saMember(s.workerSA)

	for _, b := range []struct {
		name, role string
		member     pulumi.StringInput
	}{
		{"api-sql", "roles/cloudsql.client", s.apiMember},
		{"worker-sql", "roles/cloudsql.client", s.workerMember},
		{"worker-bq-job", "roles/bigquery.jobUser", s.workerMember},
		{"worker-bq-read-session", "roles/bigquery.readSessionUser", s.workerMember},
	} {
		if _, err := projects.NewIAMMember(ctx, b.name, &projects.IAMMemberArgs{
			Project: pulumi.String(st.project), Role: pulumi.String(b.role), Member: b.member,
		}); err != nil {
			return nil, err
		}
	}
	// The API signs download URLs through IAM Credentials (no key file).
	if _, err := serviceaccount.NewIAMMember(ctx, "api-sign-self", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: s.apiSA.Name,
		Role:             pulumi.String("roles/iam.serviceAccountTokenCreator"),
		Member:           s.apiMember,
	}); err != nil {
		return nil, err
	}

	// ---- Artifact Registry + image ----------------------------------
	s.repo, err = artifactregistry.NewRepository(ctx, "images", &artifactregistry.RepositoryArgs{
		Location:     pulumi.String(st.region),
		RepositoryId: pulumi.String(appName),
		Format:       pulumi.String("DOCKER"),
	}, afterAPIs)
	if err != nil {
		return nil, err
	}
	// The image is built and pushed outside Pulumi (`make image`, or CI) and
	// passed in by digest, so every deploy pins an exact, immutable build.
	s.image = pulumi.String(st.image)
	ctx.Export("imageRepository", pulumi.Sprintf("%s-docker.pkg.dev/%s/%s/app", st.region, st.project, s.repo.RepositoryId))

	// ---- Cloud SQL Postgres -----------------------------------------
	pw, err := random.NewRandomPassword(ctx, "db-password", &random.RandomPasswordArgs{
		Length:  pulumi.Int(32),
		Special: pulumi.Bool(false),
	})
	if err != nil {
		return nil, err
	}
	s.dbPassword = pw.Result
	s.dbInstance, err = sql.NewDatabaseInstance(ctx, "postgres", &sql.DatabaseInstanceArgs{
		Region:             pulumi.String(st.region),
		DatabaseVersion:    pulumi.String("POSTGRES_16"),
		DeletionProtection: pulumi.Bool(st.deletionProtection),
		Settings: &sql.DatabaseInstanceSettingsArgs{
			Tier:    pulumi.String(st.dbTier),
			Edition: pulumi.String("ENTERPRISE"),
			// Public IP with no authorized networks: only the Cloud SQL
			// connector / Cloud Run integration can connect (mTLS + IAM).
			IpConfiguration: &sql.DatabaseInstanceSettingsIpConfigurationArgs{
				Ipv4Enabled: pulumi.Bool(true),
			},
			BackupConfiguration: &sql.DatabaseInstanceSettingsBackupConfigurationArgs{
				Enabled:                    pulumi.Bool(true),
				PointInTimeRecoveryEnabled: pulumi.Bool(true),
			},
			DeletionProtectionEnabled: pulumi.Bool(st.deletionProtection),
		},
	}, afterAPIs)
	if err != nil {
		return nil, err
	}
	db, err := sql.NewDatabase(ctx, "app", &sql.DatabaseArgs{
		Instance: s.dbInstance.Name,
		Name:     pulumi.String("app"),
	})
	if err != nil {
		return nil, err
	}
	user, err := sql.NewUser(ctx, "app", &sql.UserArgs{
		Instance: s.dbInstance.Name,
		Name:     pulumi.String("app"),
		Password: pw.Result,
	})
	if err != nil {
		return nil, err
	}
	s.dbName, s.dbUser = db.Name, user.Name

	// ---- Secrets ----------------------------------------------------
	if s.dbSecret, err = newSecret(ctx, "db-password", pw.Result, afterAPIs); err != nil {
		return nil, err
	}
	if st.useSendGrid {
		if s.sendgridSecret, err = newSecret(ctx, "sendgrid-api-key", st.sendgridKey, afterAPIs); err != nil {
			return nil, err
		}
	}
	grants := []struct {
		name   string
		secret *secretmanager.Secret
		member pulumi.StringInput
	}{
		{"api-reads-db-password", s.dbSecret, s.apiMember},
		{"worker-reads-db-password", s.dbSecret, s.workerMember},
	}
	if s.sendgridSecret != nil {
		grants = append(grants, struct {
			name   string
			secret *secretmanager.Secret
			member pulumi.StringInput
		}{"worker-reads-sendgrid", s.sendgridSecret, s.workerMember})
	}
	for _, g := range grants {
		r, err := secretmanager.NewSecretIamMember(ctx, g.name, &secretmanager.SecretIamMemberArgs{
			SecretId: g.secret.ID(),
			Role:     pulumi.String("roles/secretmanager.secretAccessor"),
			Member:   g.member,
		})
		if err != nil {
			return nil, err
		}
		s.secretGrants = append(s.secretGrants, r)
	}

	// ---- Cloud Storage: generated reports ----------------------------
	s.bucket, err = storage.NewBucket(ctx, "reports", &storage.BucketArgs{
		Name:                     pulumi.Sprintf("%s-%s-reports", st.project, appName),
		Location:                 pulumi.String(st.region),
		UniformBucketLevelAccess: pulumi.Bool(true),
		PublicAccessPrevention:   pulumi.String("enforced"),
		ForceDestroy:             pulumi.Bool(!st.deletionProtection),
		// Reports are regenerated on demand; don't keep them forever.
		LifecycleRules: storage.BucketLifecycleRuleArray{
			&storage.BucketLifecycleRuleArgs{
				Action:    &storage.BucketLifecycleRuleActionArgs{Type: pulumi.String("Delete")},
				Condition: &storage.BucketLifecycleRuleConditionArgs{Age: pulumi.Int(30)},
			},
		},
	}, afterAPIs)
	if err != nil {
		return nil, err
	}
	for name, b := range map[string]struct {
		role   string
		member pulumi.StringInput
	}{
		"worker-writes-reports": {"roles/storage.objectAdmin", s.workerMember},
		"api-reads-reports":     {"roles/storage.objectViewer", s.apiMember},
	} {
		if _, err := storage.NewBucketIAMMember(ctx, name, &storage.BucketIAMMemberArgs{
			Bucket: s.bucket.Name, Role: pulumi.String(b.role), Member: b.member,
		}); err != nil {
			return nil, err
		}
	}

	// ---- BigQuery: analytics events ---------------------------------
	s.dataset, err = bigquery.NewDataset(ctx, "analytics", &bigquery.DatasetArgs{
		DatasetId:               pulumi.String("analytics"),
		Location:                pulumi.String(st.bqLocation),
		DeleteContentsOnDestroy: pulumi.Bool(!st.deletionProtection),
	}, afterAPIs)
	if err != nil {
		return nil, err
	}
	// Column names/types match messages.Event in the app. Partitioned by day
	// and clustered by type so report queries scan only what they need.
	s.eventsTable, err = bigquery.NewTable(ctx, "events", &bigquery.TableArgs{
		DatasetId:          s.dataset.DatasetId,
		TableId:            pulumi.String("events"),
		DeletionProtection: pulumi.Bool(st.deletionProtection),
		TimePartitioning: &bigquery.TableTimePartitioningArgs{
			Type:  pulumi.String("DAY"),
			Field: pulumi.String("occurred_at"),
		},
		Clusterings: pulumi.StringArray{pulumi.String("event_type")},
		Schema: pulumi.String(`[
  {"name": "event_id",    "type": "STRING",    "mode": "REQUIRED"},
  {"name": "event_type",  "type": "STRING",    "mode": "REQUIRED"},
  {"name": "user_id",     "type": "STRING",    "mode": "REQUIRED"},
  {"name": "occurred_at", "type": "TIMESTAMP", "mode": "REQUIRED"},
  {"name": "received_at", "type": "TIMESTAMP", "mode": "REQUIRED"},
  {"name": "properties",  "type": "STRING",    "mode": "NULLABLE", "description": "JSON-encoded object"}
]`),
	})
	if err != nil {
		return nil, err
	}
	for name, b := range map[string]struct {
		role   string
		member pulumi.StringInput
	}{
		"worker-reads-analytics":    {"roles/bigquery.dataViewer", s.workerMember},
		"pubsub-writes-analytics":   {"roles/bigquery.dataEditor", s.pubsubAgent.Member},
		"pubsub-metadata-analytics": {"roles/bigquery.metadataViewer", s.pubsubAgent.Member},
	} {
		g, err := bigquery.NewDatasetIamMember(ctx, name, &bigquery.DatasetIamMemberArgs{
			DatasetId: s.dataset.DatasetId, Role: pulumi.String(b.role), Member: b.member,
		})
		if err != nil {
			return nil, err
		}
		s.bqGrants = append(s.bqGrants, g)
	}

	// ---- Pub/Sub topics ---------------------------------------------
	newTopic := func(name string) (*pubsub.Topic, error) {
		return pubsub.NewTopic(ctx, name, &pubsub.TopicArgs{
			Name:                     pulumi.String(name),
			MessageRetentionDuration: pulumi.String("86400s"),
		}, afterAPIs)
	}
	if s.reportsTopic, err = newTopic("report-requests"); err != nil {
		return nil, err
	}
	if s.emailsTopic, err = newTopic("email-requests"); err != nil {
		return nil, err
	}
	if s.eventsTopic, err = newTopic("events"); err != nil {
		return nil, err
	}
	if s.deadLetterTopic, err = newTopic("dead-letter"); err != nil {
		return nil, err
	}
	// Pull subscription so dead letters are retained and inspectable:
	//   gcloud pubsub subscriptions pull dead-letter-inspect --limit 10
	if _, err := pubsub.NewSubscription(ctx, "dead-letter-inspect", &pubsub.SubscriptionArgs{
		Name:                     pulumi.String("dead-letter-inspect"),
		Topic:                    s.deadLetterTopic.Name,
		MessageRetentionDuration: pulumi.String("604800s"),
		ExpirationPolicy:         &pubsub.SubscriptionExpirationPolicyArgs{Ttl: pulumi.String("")},
	}); err != nil {
		return nil, err
	}
	if _, err := pubsub.NewTopicIAMMember(ctx, "pubsub-agent-publishes-dlq", &pubsub.TopicIAMMemberArgs{
		Topic: s.deadLetterTopic.Name, Role: pulumi.String("roles/pubsub.publisher"), Member: s.pubsubAgent.Member,
	}); err != nil {
		return nil, err
	}
	// Publishers. The worker's outbox sweeper may publish report requests too.
	for name, b := range map[string]struct {
		topic  *pubsub.Topic
		member pulumi.StringInput
	}{
		"api-publishes-reports":    {s.reportsTopic, s.apiMember},
		"api-publishes-events":     {s.eventsTopic, s.apiMember},
		"worker-publishes-reports": {s.reportsTopic, s.workerMember},
		"worker-publishes-emails":  {s.emailsTopic, s.workerMember},
	} {
		if _, err := pubsub.NewTopicIAMMember(ctx, name, &pubsub.TopicIAMMemberArgs{
			Topic: b.topic.Name, Role: pulumi.String("roles/pubsub.publisher"), Member: b.member,
		}); err != nil {
			return nil, err
		}
	}

	// Events go straight from Pub/Sub into BigQuery: no consumer code.
	eventsSub, err := pubsub.NewSubscription(ctx, "events-to-bigquery", &pubsub.SubscriptionArgs{
		Name:             pulumi.String("events-to-bigquery"),
		Topic:            s.eventsTopic.Name,
		ExpirationPolicy: &pubsub.SubscriptionExpirationPolicyArgs{Ttl: pulumi.String("")},
		BigqueryConfig: &pubsub.SubscriptionBigqueryConfigArgs{
			Table:             pulumi.Sprintf("%s.%s.%s", st.project, s.dataset.DatasetId, s.eventsTable.TableId),
			UseTableSchema:    pulumi.Bool(true),
			DropUnknownFields: pulumi.Bool(true),
		},
		DeadLetterPolicy: s.deadLetterPolicy(emailsMaxAttempts),
	}, pulumi.DependsOn(s.bqGrants))
	if err != nil {
		return nil, err
	}
	if err := s.allowDeadLettering(ctx, "events-to-bigquery", eventsSub); err != nil {
		return nil, err
	}

	if s.fb, err = setupFirebase(ctx, s); err != nil {
		return nil, err
	}

	ctx.Export("platform", pulumi.String(st.platform))
	ctx.Export("reportsBucket", s.bucket.Name)
	ctx.Export("eventsTable", pulumi.Sprintf("%s.%s.%s", st.project, s.dataset.DatasetId, s.eventsTable.TableId))
	ctx.Export("dbConnectionName", s.dbInstance.ConnectionName)
	ctx.Export("image", s.image)
	return s, nil
}

func (s *shared) deadLetterPolicy(maxAttempts int) *pubsub.SubscriptionDeadLetterPolicyArgs {
	return &pubsub.SubscriptionDeadLetterPolicyArgs{
		DeadLetterTopic:     s.deadLetterTopic.ID(),
		MaxDeliveryAttempts: pulumi.Int(maxAttempts),
	}
}

func retryPolicy() *pubsub.SubscriptionRetryPolicyArgs {
	return &pubsub.SubscriptionRetryPolicyArgs{
		MinimumBackoff: pulumi.String("10s"),
		MaximumBackoff: pulumi.String("600s"),
	}
}

// allowDeadLettering lets Pub/Sub's agent ack the original message after
// forwarding it to the dead-letter topic.
func (s *shared) allowDeadLettering(ctx *pulumi.Context, name string, sub *pubsub.Subscription) error {
	_, err := pubsub.NewSubscriptionIAMMember(ctx, name+"-agent-subscriber", &pubsub.SubscriptionIAMMemberArgs{
		Subscription: sub.Name, Role: pulumi.String("roles/pubsub.subscriber"), Member: s.pubsubAgent.Member,
	})
	return err
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func newSA(ctx *pulumi.Context, id, display string, opts ...pulumi.ResourceOption) (*serviceaccount.Account, error) {
	return serviceaccount.NewAccount(ctx, id, &serviceaccount.AccountArgs{
		AccountId:   pulumi.String(appName + "-" + id),
		DisplayName: pulumi.String(display),
	}, opts...)
}

func saMember(sa *serviceaccount.Account) pulumi.StringOutput {
	return pulumi.Sprintf("serviceAccount:%s", sa.Email)
}

func newSecret(ctx *pulumi.Context, id string, value pulumi.StringInput, opts ...pulumi.ResourceOption) (*secretmanager.Secret, error) {
	s, err := secretmanager.NewSecret(ctx, id, &secretmanager.SecretArgs{
		SecretId: pulumi.String(appName + "-" + id),
		Replication: &secretmanager.SecretReplicationArgs{
			Auto: &secretmanager.SecretReplicationAutoArgs{},
		},
	}, opts...)
	if err != nil {
		return nil, err
	}
	_, err = secretmanager.NewSecretVersion(ctx, id, &secretmanager.SecretVersionArgs{
		Secret:     s.ID(),
		SecretData: value,
	})
	return s, err
}
