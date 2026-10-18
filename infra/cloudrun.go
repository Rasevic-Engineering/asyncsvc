package main

import (
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudrunv2"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudscheduler"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// deployCloudRun runs the API and worker on Cloud Run. Pub/Sub pushes to the
// worker with OIDC auth, and Cloud Scheduler triggers the outbox sweep
// (Cloud Run instances get no CPU between requests, so no background loops).
func deployCloudRun(ctx *pulumi.Context, s *shared) error {
	// Identity Pub/Sub push and Cloud Scheduler use to call the worker.
	invokerSA, err := newSA(ctx, "invoker", "Invokes the worker (Pub/Sub push, Scheduler)", pulumi.DependsOn(s.apis))
	if err != nil {
		return err
	}
	// Pub/Sub mints OIDC tokens as the invoker SA for authenticated push.
	if _, err := serviceaccount.NewIAMMember(ctx, "pubsub-agent-token-creator", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: invokerSA.Name,
		Role:             pulumi.String("roles/iam.serviceAccountTokenCreator"),
		Member:           s.pubsubAgent.Member,
	}); err != nil {
		return err
	}

	commonEnv := cloudrunv2.ServiceTemplateContainerEnvArray{
		runEnv("GCP_PROJECT", pulumi.String(s.project)),
		runEnv("DB_HOST", pulumi.Sprintf("/cloudsql/%s", s.dbInstance.ConnectionName)),
		runEnv("DB_NAME", s.dbName),
		runEnv("DB_USER", s.dbUser),
		runSecretEnv("DB_PASSWORD", s.dbSecret),
		runEnv("TOPIC_REPORTS", s.reportsTopic.Name),
		runEnv("TOPIC_EMAILS", s.emailsTopic.Name),
		runEnv("TOPIC_EVENTS", s.eventsTopic.Name),
		runEnv("TOPIC_ACTIVITY", s.activityTopic.Name),
		runEnv("REPORTS_BUCKET", s.bucket.Name),
		runEnv("IMAGE", s.image), // reported by GET /version
	}
	// Cloud Run's built-in Cloud SQL integration: a unix socket under /cloudsql.
	volumes := cloudrunv2.ServiceTemplateVolumeArray{
		&cloudrunv2.ServiceTemplateVolumeArgs{
			Name: pulumi.String("cloudsql"),
			CloudSqlInstance: &cloudrunv2.ServiceTemplateVolumeCloudSqlInstanceArgs{
				Instances: pulumi.StringArray{s.dbInstance.ConnectionName},
			},
		},
	}
	mounts := cloudrunv2.ServiceTemplateContainerVolumeMountArray{
		&cloudrunv2.ServiceTemplateContainerVolumeMountArgs{
			Name: pulumi.String("cloudsql"), MountPath: pulumi.String("/cloudsql"),
		},
	}
	startup := &cloudrunv2.ServiceTemplateContainerStartupProbeArgs{
		HttpGet: &cloudrunv2.ServiceTemplateContainerStartupProbeHttpGetArgs{Path: pulumi.String("/healthz")},
	}
	deps := append(append([]pulumi.Resource{}, s.apis...), s.secretGrants...)

	apiEnv := append(cloudrunv2.ServiceTemplateContainerEnvArray{}, commonEnv...)
	for _, e := range s.apiAuthEnv() {
		apiEnv = append(apiEnv, runEnv(e.name, e.value))
	}

	apiSvc, err := cloudrunv2.NewService(ctx, "api", &cloudrunv2.ServiceArgs{
		Name:               pulumi.String(appName + "-api"),
		Location:           pulumi.String(s.region),
		Ingress:            pulumi.String("INGRESS_TRAFFIC_ALL"),
		DeletionProtection: pulumi.Bool(false),
		Template: &cloudrunv2.ServiceTemplateArgs{
			ServiceAccount: s.apiSA.Email,
			Timeout:        pulumi.String("30s"),
			Scaling: &cloudrunv2.ServiceTemplateScalingArgs{
				MinInstanceCount: pulumi.Int(0),
				MaxInstanceCount: pulumi.Int(10),
			},
			Volumes: volumes,
			Containers: cloudrunv2.ServiceTemplateContainerArray{
				&cloudrunv2.ServiceTemplateContainerArgs{
					Image:        s.image,
					Commands:     pulumi.StringArray{pulumi.String("/bin/api")},
					Envs:         apiEnv,
					VolumeMounts: mounts,
					StartupProbe: startup,
					Resources: &cloudrunv2.ServiceTemplateContainerResourcesArgs{
						Limits: pulumi.StringMap{"cpu": pulumi.String("1"), "memory": pulumi.String("512Mi")},
					},
				},
			},
		},
	}, pulumi.DependsOn(deps))
	if err != nil {
		return err
	}

	// The URL users see: the custom domain if one is configured.
	publicURL := apiSvc.Uri
	if s.customDomain != "" {
		publicURL = pulumi.String("https://" + s.customDomain).ToStringOutput()
	}

	workerEnv := append(cloudrunv2.ServiceTemplateContainerEnvArray{}, commonEnv...)
	workerEnv = append(workerEnv,
		runEnv("WORKER_MODE", pulumi.String("push")),
		runEnv("REPORT_SOURCE", pulumi.String("bigquery")),
		runEnv("BQ_DATASET", s.dataset.DatasetId),
		runEnv("BQ_EVENTS_TABLE", s.eventsTable.TableId),
		runEnv("BQ_LOCATION", pulumi.String(s.bqLocation)),
		runEnv("REPORTS_MAX_ATTEMPTS", pulumi.Sprintf("%d", reportsMaxAttempts)),
		runEnv("EMAIL_MAX_ATTEMPTS", pulumi.Sprintf("%d", emailsMaxAttempts)),
		// Bounded by Pub/Sub's 600s max ack deadline for push.
		runEnv("REPORT_TIMEOUT", pulumi.String("9m")),
		runEnv("REPORT_STALE_AFTER", pulumi.String("15m")),
		runEnv("PUBLIC_API_URL", publicURL), // links in report emails
		runEnv("MAIL_FROM", pulumi.String(s.mailFrom)),
	)
	if s.sendgridSecret != nil {
		workerEnv = append(workerEnv, runEnv("MAIL_PROVIDER", pulumi.String("sendgrid")), runSecretEnv("SENDGRID_API_KEY", s.sendgridSecret))
	} else {
		workerEnv = append(workerEnv, runEnv("MAIL_PROVIDER", pulumi.String("log")))
	}

	workerSvc, err := cloudrunv2.NewService(ctx, "worker", &cloudrunv2.ServiceArgs{
		Name:     pulumi.String(appName + "-worker"),
		Location: pulumi.String(s.region),
		// Reachable by Pub/Sub push and Scheduler, but every request must
		// carry an OIDC token for an identity with run.invoker.
		Ingress:            pulumi.String("INGRESS_TRAFFIC_ALL"),
		DeletionProtection: pulumi.Bool(false),
		Template: &cloudrunv2.ServiceTemplateArgs{
			ServiceAccount: s.workerSA.Email,
			// Longer than the 600s ack deadline so a slow report finishes
			// cleanly instead of being cut off mid-upload.
			Timeout:                       pulumi.String("900s"),
			MaxInstanceRequestConcurrency: pulumi.Int(8),
			Scaling: &cloudrunv2.ServiceTemplateScalingArgs{
				MinInstanceCount: pulumi.Int(0),
				MaxInstanceCount: pulumi.Int(10),
			},
			Volumes: volumes,
			Containers: cloudrunv2.ServiceTemplateContainerArray{
				&cloudrunv2.ServiceTemplateContainerArgs{
					Image:        s.image,
					Commands:     pulumi.StringArray{pulumi.String("/bin/worker")},
					Envs:         workerEnv,
					VolumeMounts: mounts,
					StartupProbe: startup,
					Resources: &cloudrunv2.ServiceTemplateContainerResourcesArgs{
						Limits: pulumi.StringMap{"cpu": pulumi.String("1"), "memory": pulumi.String("1Gi")},
					},
				},
			},
		},
	}, pulumi.DependsOn(append(deps, s.bqGrants...)))
	if err != nil {
		return err
	}

	workerInvoker, err := cloudrunv2.NewServiceIamMember(ctx, "invoker-calls-worker", &cloudrunv2.ServiceIamMemberArgs{
		Name:     workerSvc.Name,
		Location: workerSvc.Location,
		Role:     pulumi.String("roles/run.invoker"),
		Member:   saMember(invokerSA),
	})
	if err != nil {
		return err
	}
	if s.publicAPI {
		// The sample API has no auth of its own. Put it behind API Gateway,
		// IAP, or your own auth middleware before exposing real data.
		if _, err := cloudrunv2.NewServiceIamMember(ctx, "public-calls-api", &cloudrunv2.ServiceIamMemberArgs{
			Name:     apiSvc.Name,
			Location: apiSvc.Location,
			Role:     pulumi.String("roles/run.invoker"),
			Member:   pulumi.String("allUsers"),
		}); err != nil {
			return err
		}
	}

	// ---- Push subscriptions -----------------------------------------
	pushSub := func(name string, topic *pubsub.Topic, path string, ackDeadline, maxAttempts int) error {
		sub, err := pubsub.NewSubscription(ctx, name, &pubsub.SubscriptionArgs{
			Name:               pulumi.String(name),
			Topic:              topic.Name,
			AckDeadlineSeconds: pulumi.Int(ackDeadline),
			ExpirationPolicy:   &pubsub.SubscriptionExpirationPolicyArgs{Ttl: pulumi.String("")},
			PushConfig: &pubsub.SubscriptionPushConfigArgs{
				PushEndpoint: pulumi.Sprintf("%s%s", workerSvc.Uri, path),
				OidcToken: &pubsub.SubscriptionPushConfigOidcTokenArgs{
					ServiceAccountEmail: invokerSA.Email,
					Audience:            workerSvc.Uri,
				},
			},
			RetryPolicy:      retryPolicy(),
			DeadLetterPolicy: s.deadLetterPolicy(maxAttempts),
		}, pulumi.DependsOn([]pulumi.Resource{workerInvoker}))
		if err != nil {
			return err
		}
		return s.allowDeadLettering(ctx, name, sub)
	}
	// 600s is Pub/Sub's max ack deadline: reports can take minutes.
	if err := pushSub("reports-push", s.reportsTopic, "/pubsub/reports", 600, reportsMaxAttempts); err != nil {
		return err
	}
	if err := pushSub("emails-push", s.emailsTopic, "/pubsub/emails", 60, emailsMaxAttempts); err != nil {
		return err
	}
	// Activity events: the worker stores them in Firestore for the live feed.
	if err := pushSub("activity-push", s.activityTopic, "/pubsub/activity", 30, emailsMaxAttempts); err != nil {
		return err
	}

	// ---- Cloud Scheduler: outbox sweeper ----------------------------
	if _, err := cloudscheduler.NewJob(ctx, "relay-outbox", &cloudscheduler.JobArgs{
		Name:            pulumi.String(appName + "-relay-outbox"),
		Region:          pulumi.String(s.region),
		Schedule:        pulumi.String("* * * * *"),
		TimeZone:        pulumi.String("Etc/UTC"),
		AttemptDeadline: pulumi.String("120s"),
		HttpTarget: &cloudscheduler.JobHttpTargetArgs{
			Uri:        pulumi.Sprintf("%s/tasks/relay-outbox", workerSvc.Uri),
			HttpMethod: pulumi.String("POST"),
			OidcToken: &cloudscheduler.JobHttpTargetOidcTokenArgs{
				ServiceAccountEmail: invokerSA.Email,
				Audience:            workerSvc.Uri,
			},
		},
	}, pulumi.DependsOn([]pulumi.Resource{workerInvoker})); err != nil {
		return err
	}

	// Sign-in config; the API's own hostname must be an authorized domain.
	// Sign-in config; the API's own hostnames must be authorized domains.
	apiHost := apiSvc.Uri.ApplyT(func(u string) string { return strings.TrimPrefix(u, "https://") }).(pulumi.StringOutput)
	hosts := []pulumi.StringInput{apiHost}
	if s.customDomain != "" {
		hosts = append(hosts, pulumi.String(s.customDomain))
	}
	if err := s.fb.configureSignIn(ctx, s, hosts...); err != nil {
		return err
	}

	if s.customDomain != "" {
		if err := setupLoadBalancer(ctx, s, apiSvc); err != nil {
			return err
		}
	}

	ctx.Export("apiUrl", publicURL)
	ctx.Export("runUrl", apiSvc.Uri)
	ctx.Export("workerUrl", workerSvc.Uri)
	return nil
}

func runEnv(name string, value pulumi.StringInput) *cloudrunv2.ServiceTemplateContainerEnvArgs {
	return &cloudrunv2.ServiceTemplateContainerEnvArgs{Name: pulumi.String(name), Value: value}
}

func runSecretEnv(name string, s *secretmanager.Secret) *cloudrunv2.ServiceTemplateContainerEnvArgs {
	return &cloudrunv2.ServiceTemplateContainerEnvArgs{
		Name: pulumi.String(name),
		ValueSource: &cloudrunv2.ServiceTemplateContainerEnvValueSourceArgs{
			SecretKeyRef: &cloudrunv2.ServiceTemplateContainerEnvValueSourceSecretKeyRefArgs{
				Secret:  s.SecretId,
				Version: pulumi.String("latest"),
			},
		},
	}
}
