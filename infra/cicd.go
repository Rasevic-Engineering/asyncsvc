package main

import (
	"fmt"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/iam"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// githubDeploy says which GitHub Actions runs may deploy this stack.
type githubDeploy struct {
	repository  string // owner/name
	environment string // GitHub environment the deploy job runs in
	branch      string // branch whose runs may deploy
}

// setupGitHubDeploys lets GitHub Actions deploy this stack without a key file,
// via Workload Identity Federation: GitHub's OIDC token for a workflow run is
// exchanged for short-lived credentials of a deployer service account.
//
// Only runs of one repository, in one GitHub environment, on one branch are
// trusted. The environment is where approvals live (required reviewers in
// the repo's Settings > Environments), and pull request runs can't obtain
// GCP credentials at all. Each stack (dev, staging, prod) gets its own
// environment and deployer, so one environment can never deploy another.
func setupGitHubDeploys(ctx *pulumi.Context, s *shared, gh githubDeploy) error {
	afterAPIs := pulumi.DependsOn(s.apis)

	pool, err := iam.NewWorkloadIdentityPool(ctx, "github", &iam.WorkloadIdentityPoolArgs{
		Project:                pulumi.String(s.project),
		WorkloadIdentityPoolId: pulumi.String("github"),
		DisplayName:            pulumi.String("GitHub Actions"),
	}, afterAPIs)
	if err != nil {
		return err
	}
	provider, err := iam.NewWorkloadIdentityPoolProvider(ctx, "github", &iam.WorkloadIdentityPoolProviderArgs{
		Project:                        pulumi.String(s.project),
		WorkloadIdentityPoolId:         pool.WorkloadIdentityPoolId,
		WorkloadIdentityPoolProviderId: pulumi.String("github-actions"),
		DisplayName:                    pulumi.String("GitHub Actions OIDC"),
		AttributeMapping: pulumi.StringMap{
			"google.subject":        pulumi.String("assertion.sub"),
			"attribute.repository":  pulumi.String("assertion.repository"),
			"attribute.ref":         pulumi.String("assertion.ref"),
			"attribute.environment": pulumi.String("assertion.environment"),
		},
		AttributeCondition: pulumi.String(fmt.Sprintf(
			"assertion.repository == '%s' && assertion.environment == '%s' && assertion.ref == 'refs/heads/%s'",
			gh.repository, gh.environment, gh.branch)),
		Oidc: &iam.WorkloadIdentityPoolProviderOidcArgs{
			IssuerUri: pulumi.String("https://token.actions.githubusercontent.com"),
		},
	})
	if err != nil {
		return err
	}

	deployer, err := newSA(ctx, "deployer", "Deploys asyncsvc from GitHub Actions", afterAPIs)
	if err != nil {
		return err
	}
	// Pulumi manages APIs, IAM bindings and service accounts for the whole
	// stack, and any identity allowed to edit project IAM can grant itself
	// owner anyway, so a narrower role set wouldn't add real protection.
	// What limits this identity is the provider condition above.
	if _, err := projects.NewIAMMember(ctx, "deployer-owner", &projects.IAMMemberArgs{
		Project: pulumi.String(s.project),
		Role:    pulumi.String("roles/owner"),
		Member:  saMember(deployer),
	}); err != nil {
		return err
	}
	if _, err := serviceaccount.NewIAMMember(ctx, "github-impersonates-deployer", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: deployer.Name,
		Role:             pulumi.String("roles/iam.workloadIdentityUser"),
		Member:           pulumi.Sprintf("principalSet://iam.googleapis.com/%s/attribute.environment/%s", pool.Name, gh.environment),
	}); err != nil {
		return err
	}

	ctx.Export("githubWorkloadIdentityProvider", provider.Name)
	ctx.Export("githubDeployerServiceAccount", deployer.Email)
	ctx.Export("githubEnvironment", pulumi.String(gh.environment))
	return nil
}
