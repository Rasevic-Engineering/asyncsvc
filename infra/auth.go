package main

import (
	"strconv"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/firebase"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/identityplatform"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// firebaseAuth holds the Firebase / Identity Platform pieces the API and the
// web app need: Firebase enabled on the project, a registered web app (whose
// public apiKey/authDomain the browser uses), and sign-in configuration.
type firebaseAuth struct {
	provider   *gcp.Provider
	project    *firebase.Project
	apiKey     pulumi.StringOutput
	authDomain pulumi.StringOutput
}

func setupFirebase(ctx *pulumi.Context, s *shared) (*firebaseAuth, error) {
	// Firebase management APIs reject user credentials (gcloud ADC) unless
	// requests are billed to a quota project, so use a dedicated provider.
	provider, err := gcp.NewProvider(ctx, "firebase", &gcp.ProviderArgs{
		Project:             pulumi.String(s.project),
		BillingProject:      pulumi.String(s.project),
		UserProjectOverride: pulumi.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	withProvider := pulumi.Provider(provider)

	project, err := firebase.NewProject(ctx, "firebase", &firebase.ProjectArgs{
		Project: pulumi.String(s.project),
	}, withProvider, pulumi.DependsOn(s.apis))
	if err != nil {
		return nil, err
	}
	webApp, err := firebase.NewWebApp(ctx, "web", &firebase.WebAppArgs{
		Project:        pulumi.String(s.project),
		DisplayName:    pulumi.String("Async Reports"),
		DeletionPolicy: pulumi.String("DELETE"),
	}, withProvider, pulumi.DependsOn([]pulumi.Resource{project}))
	if err != nil {
		return nil, err
	}
	cfg := firebase.GetWebAppConfigOutput(ctx, firebase.GetWebAppConfigOutputArgs{
		Project:  pulumi.String(s.project),
		WebAppId: webApp.AppId,
	}, withProvider)

	return &firebaseAuth{
		provider:   provider,
		project:    project,
		apiKey:     cfg.ApiKey(),
		authDomain: cfg.AuthDomain(),
	}, nil
}

// configureSignIn enables Identity Platform with email/password sign-in, and
// Google sign-in when an OAuth client is configured. apiHosts are added to
// the authorized domains (needed for the Google sign-in popup).
func (a *firebaseAuth) configureSignIn(ctx *pulumi.Context, s *shared, apiHosts ...pulumi.StringInput) error {
	domains := pulumi.StringArray{
		pulumi.String("localhost"),
		pulumi.Sprintf("%s.firebaseapp.com", s.project),
		pulumi.Sprintf("%s.web.app", s.project),
	}
	for _, h := range apiHosts {
		domains = append(domains, h)
	}

	cfg, err := identityplatform.NewConfig(ctx, "auth", &identityplatform.ConfigArgs{
		Project: pulumi.String(s.project),
		SignIn: &identityplatform.ConfigSignInArgs{
			AllowDuplicateEmails: pulumi.Bool(false),
			Email: &identityplatform.ConfigSignInEmailArgs{
				Enabled:          pulumi.Bool(true),
				PasswordRequired: pulumi.Bool(true),
			},
		},
		AuthorizedDomains: domains,
	}, pulumi.Provider(a.provider), pulumi.DependsOn([]pulumi.Resource{a.project}))
	if err != nil {
		return err
	}

	if s.useGoogleSignIn {
		// The OAuth client itself must be created once in the console
		// (APIs & Services > Credentials); Google doesn't allow creating
		// it through the API.
		if _, err := identityplatform.NewDefaultSupportedIdpConfig(ctx, "google", &identityplatform.DefaultSupportedIdpConfigArgs{
			Project:      pulumi.String(s.project),
			IdpId:        pulumi.String("google.com"),
			ClientId:     pulumi.String(s.googleClientID),
			ClientSecret: s.googleClientSecret,
			Enabled:      pulumi.Bool(true),
		}, pulumi.Provider(a.provider), pulumi.DependsOn([]pulumi.Resource{cfg})); err != nil {
			return err
		}
	}
	return nil
}

type envVar struct {
	name  string
	value pulumi.StringInput
}

// apiAuthEnv is the API's auth configuration, shared by both platforms.
func (s *shared) apiAuthEnv() []envVar {
	return []envVar{
		{"FIREBASE_PROJECT_ID", pulumi.String(s.project)},
		{"ADMIN_EMAILS", pulumi.String(s.adminEmails)},
		{"WEB_FIREBASE_API_KEY", s.fb.apiKey},
		{"WEB_FIREBASE_AUTH_DOMAIN", s.fb.authDomain},
		{"WEB_GOOGLE_SIGN_IN", pulumi.String(strconv.FormatBool(s.useGoogleSignIn))},
	}
}
