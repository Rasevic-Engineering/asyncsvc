package main

import (
	"fmt"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/container"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	appsv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apps/v1"
	autoscalingv2 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/autoscaling/v2"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	policyv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/policy/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const namespace = appName

// deployGKE runs the API and worker on a GKE Autopilot cluster.
//
// Differences from Cloud Run:
//   - The worker uses streaming PULL. No public endpoint is needed, and the
//     client extends ack deadlines, so reports can run past 10 minutes.
//   - The outbox sweep runs inside the worker (pods have CPU all the time).
//   - Pods reach Cloud SQL through the Cloud SQL Go connector and act as the
//     api/worker Google service accounts via Workload Identity (no keys).
func deployGKE(ctx *pulumi.Context, s *shared) error {
	afterAPIs := pulumi.DependsOn(s.apis)

	// ---- Cluster ----------------------------------------------------
	// Dedicated node service account instead of the Compute Engine default.
	nodeSA, err := newSA(ctx, "gke-nodes", "GKE Autopilot nodes", afterAPIs)
	if err != nil {
		return err
	}
	if _, err := projects.NewIAMMember(ctx, "gke-nodes-default-role", &projects.IAMMemberArgs{
		Project: pulumi.String(s.project),
		Role:    pulumi.String("roles/container.defaultNodeServiceAccount"),
		Member:  saMember(nodeSA),
	}); err != nil {
		return err
	}
	if _, err := artifactregistry.NewRepositoryIamMember(ctx, "gke-nodes-pull-images", &artifactregistry.RepositoryIamMemberArgs{
		Location:   s.repo.Location,
		Repository: s.repo.Name,
		Role:       pulumi.String("roles/artifactregistry.reader"),
		Member:     saMember(nodeSA),
	}); err != nil {
		return err
	}

	cluster, err := container.NewCluster(ctx, "gke", &container.ClusterArgs{
		Name:               pulumi.String(appName),
		Location:           pulumi.String(s.region),
		EnableAutopilot:    pulumi.Bool(true),
		DeletionProtection: pulumi.Bool(s.deletionProtection),
		ReleaseChannel:     &container.ClusterReleaseChannelArgs{Channel: pulumi.String("REGULAR")},
		// VPC-native networking (required by Autopilot), auto-assigned ranges.
		IpAllocationPolicy: &container.ClusterIpAllocationPolicyArgs{},
		ClusterAutoscaling: &container.ClusterClusterAutoscalingArgs{
			AutoProvisioningDefaults: &container.ClusterClusterAutoscalingAutoProvisioningDefaultsArgs{
				ServiceAccount: nodeSA.Email,
				OauthScopes:    pulumi.StringArray{pulumi.String("https://www.googleapis.com/auth/cloud-platform")},
			},
		},
	}, afterAPIs)
	if err != nil {
		return err
	}

	k8s, err := kubernetes.NewProvider(ctx, "gke", &kubernetes.ProviderArgs{
		Kubeconfig:            kubeconfig(cluster),
		DeleteUnreachable:     pulumi.Bool(true),
		EnableServerSideApply: pulumi.Bool(true),
	})
	if err != nil {
		return err
	}
	inK8s := pulumi.Provider(k8s)

	// ---- Namespace, service accounts, Workload Identity --------------
	ns, err := corev1.NewNamespace(ctx, namespace, &corev1.NamespaceArgs{
		Metadata: &metav1.ObjectMetaArgs{Name: pulumi.String(namespace)},
	}, inK8s)
	if err != nil {
		return err
	}
	inNS := pulumi.DependsOn([]pulumi.Resource{ns})

	ksa := func(name string, gsa *serviceaccount.Account) (*corev1.ServiceAccount, error) {
		// Allow the Kubernetes SA to act as the Google SA.
		if _, err := serviceaccount.NewIAMMember(ctx, name+"-workload-identity", &serviceaccount.IAMMemberArgs{
			ServiceAccountId: gsa.Name,
			Role:             pulumi.String("roles/iam.workloadIdentityUser"),
			Member:           pulumi.Sprintf("serviceAccount:%s.svc.id.goog[%s/%s]", s.project, namespace, name),
		}, pulumi.DependsOn([]pulumi.Resource{cluster})); err != nil {
			return nil, err
		}
		return corev1.NewServiceAccount(ctx, name, &corev1.ServiceAccountArgs{
			Metadata: &metav1.ObjectMetaArgs{
				Name:        pulumi.String(name),
				Namespace:   pulumi.String(namespace),
				Annotations: pulumi.StringMap{"iam.gke.io/gcp-service-account": gsa.Email},
			},
		}, inK8s, inNS)
	}
	apiKSA, err := ksa("api", s.apiSA)
	if err != nil {
		return err
	}
	workerKSA, err := ksa("worker", s.workerSA)
	if err != nil {
		return err
	}

	// Secret Manager stays the source of truth; Pulumi mirrors the values
	// into a Kubernetes Secret so pods read them as env vars.
	secretData := pulumi.StringMap{"DB_PASSWORD": s.dbPassword}
	if s.useSendGrid {
		secretData["SENDGRID_API_KEY"] = s.sendgridKey
	}
	appSecret, err := corev1.NewSecret(ctx, "app-secrets", &corev1.SecretArgs{
		Metadata:   &metav1.ObjectMetaArgs{Name: pulumi.String("app-secrets"), Namespace: pulumi.String(namespace)},
		StringData: secretData,
	}, inK8s, inNS)
	if err != nil {
		return err
	}

	// ---- Pull subscriptions -----------------------------------------
	pullSub := func(name string, topic *pubsub.Topic, ackDeadline, maxAttempts int) error {
		sub, err := pubsub.NewSubscription(ctx, name, &pubsub.SubscriptionArgs{
			Name:               pulumi.String(name),
			Topic:              topic.Name,
			AckDeadlineSeconds: pulumi.Int(ackDeadline),
			ExpirationPolicy:   &pubsub.SubscriptionExpirationPolicyArgs{Ttl: pulumi.String("")},
			RetryPolicy:        retryPolicy(),
			DeadLetterPolicy:   s.deadLetterPolicy(maxAttempts),
		})
		if err != nil {
			return err
		}
		if _, err := pubsub.NewSubscriptionIAMMember(ctx, name+"-worker-subscriber", &pubsub.SubscriptionIAMMemberArgs{
			Subscription: sub.Name, Role: pulumi.String("roles/pubsub.subscriber"), Member: s.workerMember,
		}); err != nil {
			return err
		}
		return s.allowDeadLettering(ctx, name, sub)
	}
	// The client library extends deadlines while a handler runs, so these
	// are just the initial lease.
	if err := pullSub("reports-pull", s.reportsTopic, 600, reportsMaxAttempts); err != nil {
		return err
	}
	if err := pullSub("emails-pull", s.emailsTopic, 60, emailsMaxAttempts); err != nil {
		return err
	}

	// ---- Workloads --------------------------------------------------
	commonEnv := corev1.EnvVarArray{
		k8sEnv("GCP_PROJECT", pulumi.String(s.project)),
		k8sEnv("DB_INSTANCE_CONNECTION_NAME", s.dbInstance.ConnectionName),
		k8sEnv("DB_NAME", s.dbName),
		k8sEnv("DB_USER", s.dbUser),
		k8sSecretEnv("DB_PASSWORD", appSecret),
		k8sEnv("TOPIC_REPORTS", s.reportsTopic.Name),
		k8sEnv("TOPIC_EMAILS", s.emailsTopic.Name),
		k8sEnv("TOPIC_EVENTS", s.eventsTopic.Name),
		k8sEnv("REPORTS_BUCKET", s.bucket.Name),
	}

	apiEnv := append(corev1.EnvVarArray{}, commonEnv...)
	for _, e := range s.apiAuthEnv() {
		apiEnv = append(apiEnv, k8sEnv(e.name, e.value))
	}

	apiLabels := pulumi.StringMap{"app": pulumi.String("api")}
	apiDeploy, err := appsv1.NewDeployment(ctx, "api", &appsv1.DeploymentArgs{
		Metadata: &metav1.ObjectMetaArgs{Name: pulumi.String("api"), Namespace: pulumi.String(namespace)},
		Spec: &appsv1.DeploymentSpecArgs{
			Replicas: pulumi.Int(2),
			Selector: &metav1.LabelSelectorArgs{MatchLabels: apiLabels},
			Template: &corev1.PodTemplateSpecArgs{
				Metadata: &metav1.ObjectMetaArgs{Labels: apiLabels},
				Spec: podSpec(apiKSA, 30, &corev1.ContainerArgs{
					Name:    pulumi.String("api"),
					Image:   s.image,
					Command: pulumi.StringArray{pulumi.String("/bin/api")},
					Env:     apiEnv,
					Ports:   corev1.ContainerPortArray{&corev1.ContainerPortArgs{ContainerPort: pulumi.Int(8080), Name: pulumi.String("http")}},
					Resources: &corev1.ResourceRequirementsArgs{
						Requests: pulumi.StringMap{"cpu": pulumi.String("250m"), "memory": pulumi.String("512Mi")},
						Limits:   pulumi.StringMap{"memory": pulumi.String("512Mi")},
					},
					ReadinessProbe: httpProbe(5),
					LivenessProbe:  httpProbe(15),
				}),
			},
		},
	}, inK8s, inNS, pulumi.DependsOn(s.secretGrants))
	if err != nil {
		return err
	}

	svcAnnotations := pulumi.StringMap{}
	if !s.publicAPI {
		// Reachable only from inside the VPC.
		svcAnnotations["networking.gke.io/load-balancer-type"] = pulumi.String("Internal")
	}
	apiSvc, err := corev1.NewService(ctx, "api", &corev1.ServiceArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:        pulumi.String("api"),
			Namespace:   pulumi.String(namespace),
			Annotations: svcAnnotations,
		},
		Spec: &corev1.ServiceSpecArgs{
			Type:     pulumi.String("LoadBalancer"),
			Selector: apiLabels,
			Ports: corev1.ServicePortArray{&corev1.ServicePortArgs{
				Name: pulumi.String("http"), Port: pulumi.Int(80), TargetPort: pulumi.String("http"),
			}},
		},
	}, inK8s, inNS)
	if err != nil {
		return err
	}
	apiURL := apiSvc.Status.LoadBalancer().Ingress().Index(pulumi.Int(0)).Ip().
		ApplyT(func(ip *string) string {
			if ip == nil {
				return ""
			}
			return "http://" + *ip
		}).(pulumi.StringOutput)

	if _, err := autoscalingv2.NewHorizontalPodAutoscaler(ctx, "api", &autoscalingv2.HorizontalPodAutoscalerArgs{
		Metadata: &metav1.ObjectMetaArgs{Name: pulumi.String("api"), Namespace: pulumi.String(namespace)},
		Spec: &autoscalingv2.HorizontalPodAutoscalerSpecArgs{
			ScaleTargetRef: &autoscalingv2.CrossVersionObjectReferenceArgs{
				ApiVersion: pulumi.String("apps/v1"), Kind: pulumi.String("Deployment"), Name: apiDeploy.Metadata.Name().Elem(),
			},
			MinReplicas: pulumi.Int(2),
			MaxReplicas: pulumi.Int(10),
			Metrics: autoscalingv2.MetricSpecArray{&autoscalingv2.MetricSpecArgs{
				Type: pulumi.String("Resource"),
				Resource: &autoscalingv2.ResourceMetricSourceArgs{
					Name:   pulumi.String("cpu"),
					Target: &autoscalingv2.MetricTargetArgs{Type: pulumi.String("Utilization"), AverageUtilization: pulumi.Int(70)},
				},
			}},
		},
	}, inK8s, inNS); err != nil {
		return err
	}
	if _, err := policyv1.NewPodDisruptionBudget(ctx, "api", &policyv1.PodDisruptionBudgetArgs{
		Metadata: &metav1.ObjectMetaArgs{Name: pulumi.String("api"), Namespace: pulumi.String(namespace)},
		Spec: &policyv1.PodDisruptionBudgetSpecArgs{
			MinAvailable: pulumi.Int(1),
			Selector:     &metav1.LabelSelectorArgs{MatchLabels: apiLabels},
		},
	}, inK8s, inNS); err != nil {
		return err
	}

	workerEnv := append(corev1.EnvVarArray{}, commonEnv...)
	workerEnv = append(workerEnv,
		k8sEnv("WORKER_MODE", pulumi.String("pull")),
		k8sEnv("SUB_REPORTS", pulumi.String("reports-pull")),
		k8sEnv("SUB_EMAILS", pulumi.String("emails-pull")),
		k8sEnv("OUTBOX_SWEEP_INTERVAL", pulumi.String("30s")),
		k8sEnv("REPORT_SOURCE", pulumi.String("bigquery")),
		k8sEnv("BQ_DATASET", s.dataset.DatasetId),
		k8sEnv("BQ_EVENTS_TABLE", s.eventsTable.TableId),
		k8sEnv("BQ_LOCATION", pulumi.String(s.bqLocation)),
		k8sEnv("REPORTS_MAX_ATTEMPTS", pulumi.Sprintf("%d", reportsMaxAttempts)),
		// Pull mode isn't bound by the 600s ack deadline.
		k8sEnv("REPORT_TIMEOUT", pulumi.String("25m")),
		k8sEnv("REPORT_STALE_AFTER", pulumi.String("35m")),
		k8sEnv("PUBLIC_API_URL", apiURL),
		k8sEnv("MAIL_FROM", pulumi.String(s.mailFrom)),
	)
	if s.useSendGrid {
		workerEnv = append(workerEnv, k8sEnv("MAIL_PROVIDER", pulumi.String("sendgrid")), k8sSecretEnv("SENDGRID_API_KEY", appSecret))
	} else {
		workerEnv = append(workerEnv, k8sEnv("MAIL_PROVIDER", pulumi.String("log")))
	}

	workerLabels := pulumi.StringMap{"app": pulumi.String("worker")}
	if _, err := appsv1.NewDeployment(ctx, "worker", &appsv1.DeploymentArgs{
		Metadata: &metav1.ObjectMetaArgs{Name: pulumi.String("worker"), Namespace: pulumi.String(namespace)},
		Spec: &appsv1.DeploymentSpecArgs{
			// Fixed replica count. For queue-depth autoscaling, add KEDA with a
			// gcp-pubsub scaler on reports-pull's num_undelivered_messages.
			Replicas: pulumi.Int(s.workerReplicas),
			Selector: &metav1.LabelSelectorArgs{MatchLabels: workerLabels},
			Template: &corev1.PodTemplateSpecArgs{
				Metadata: &metav1.ObjectMetaArgs{Labels: workerLabels},
				// On SIGTERM the worker stops pulling and cancels in-flight
				// reports, which are released and redelivered to another pod.
				Spec: podSpec(workerKSA, 60, &corev1.ContainerArgs{
					Name:    pulumi.String("worker"),
					Image:   s.image,
					Command: pulumi.StringArray{pulumi.String("/bin/worker")},
					Env:     workerEnv,
					Ports:   corev1.ContainerPortArray{&corev1.ContainerPortArgs{ContainerPort: pulumi.Int(8080), Name: pulumi.String("http")}},
					Resources: &corev1.ResourceRequirementsArgs{
						Requests: pulumi.StringMap{"cpu": pulumi.String("500m"), "memory": pulumi.String("1Gi")},
						Limits:   pulumi.StringMap{"memory": pulumi.String("1Gi")},
					},
					ReadinessProbe: httpProbe(5),
					LivenessProbe:  httpProbe(15),
				}),
			},
		},
	}, inK8s, inNS, pulumi.DependsOn(append(append([]pulumi.Resource{}, s.secretGrants...), s.bqGrants...))); err != nil {
		return err
	}

	// Firebase authorized domains must be hostnames, so the load balancer's
	// bare IP isn't added. Email/password sign-in works regardless; for
	// Google sign-in, point a domain at the load balancer and add it.
	if err := s.fb.configureSignIn(ctx, s); err != nil {
		return err
	}

	ctx.Export("apiUrl", apiURL)
	ctx.Export("clusterName", cluster.Name)
	ctx.Export("getCredentials", pulumi.Sprintf("gcloud container clusters get-credentials %s --region %s --project %s", cluster.Name, s.region, s.project))
	return nil
}

// kubeconfig authenticates with gke-gcloud-auth-plugin, which uses your
// gcloud credentials. Install it with:
//
//	gcloud components install gke-gcloud-auth-plugin
func kubeconfig(c *container.Cluster) pulumi.StringOutput {
	return pulumi.All(c.Name, c.Endpoint, c.MasterAuth.ClusterCaCertificate()).ApplyT(func(a []interface{}) string {
		name, endpoint := a[0].(string), a[1].(string)
		ca := ""
		if p, ok := a[2].(*string); ok && p != nil {
			ca = *p
		}
		return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[1]s
  cluster:
    server: https://%[2]s
    certificate-authority-data: %[3]s
contexts:
- name: %[1]s
  context:
    cluster: %[1]s
    user: %[1]s
current-context: %[1]s
users:
- name: %[1]s
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
      installHint: Install with "gcloud components install gke-gcloud-auth-plugin"
      provideClusterInfo: true
`, name, endpoint, ca)
	}).(pulumi.StringOutput)
}

// podSpec applies the hardening Autopilot expects: non-root, no privilege
// escalation, read-only root filesystem, RuntimeDefault seccomp.
func podSpec(sa *corev1.ServiceAccount, gracePeriod int, c *corev1.ContainerArgs) *corev1.PodSpecArgs {
	c.SecurityContext = &corev1.SecurityContextArgs{
		AllowPrivilegeEscalation: pulumi.Bool(false),
		ReadOnlyRootFilesystem:   pulumi.Bool(true),
		Capabilities:             &corev1.CapabilitiesArgs{Drop: pulumi.StringArray{pulumi.String("ALL")}},
	}
	return &corev1.PodSpecArgs{
		ServiceAccountName:            sa.Metadata.Name().Elem(),
		TerminationGracePeriodSeconds: pulumi.Int(gracePeriod),
		SecurityContext: &corev1.PodSecurityContextArgs{
			RunAsNonRoot:   pulumi.Bool(true),
			RunAsUser:      pulumi.Int(65532), // distroless "nonroot"
			SeccompProfile: &corev1.SeccompProfileArgs{Type: pulumi.String("RuntimeDefault")},
		},
		Containers: corev1.ContainerArray{c},
	}
}

func httpProbe(period int) *corev1.ProbeArgs {
	return &corev1.ProbeArgs{
		HttpGet:       &corev1.HTTPGetActionArgs{Path: pulumi.String("/healthz"), Port: pulumi.String("http")},
		PeriodSeconds: pulumi.Int(period),
	}
}

func k8sEnv(name string, value pulumi.StringInput) corev1.EnvVarInput {
	return &corev1.EnvVarArgs{Name: pulumi.String(name), Value: value}
}

func k8sSecretEnv(key string, secret *corev1.Secret) corev1.EnvVarInput {
	return &corev1.EnvVarArgs{
		Name: pulumi.String(key),
		ValueFrom: &corev1.EnvVarSourceArgs{
			SecretKeyRef: &corev1.SecretKeySelectorArgs{Name: secret.Metadata.Name(), Key: pulumi.String(key)},
		},
	}
}
