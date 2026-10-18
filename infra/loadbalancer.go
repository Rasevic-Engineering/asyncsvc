package main

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudrunv2"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/compute"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// setupLoadBalancer serves the API (and web app) at s.customDomain through a
// global external Application Load Balancer:
//
//	DNS A record ──▶ static IP ──▶ :443 HTTPS proxy (managed cert) ──▶ URL map
//	                          └──▶ :80  redirect to HTTPS
//	URL map ──▶ backend service ──▶ serverless NEG ──▶ Cloud Run api
//
// The Google-managed certificate is issued once the domain's A record points
// at the static IP (exported as customDomainDns); no other ownership check
// is needed. Issuing takes from about 15 minutes to a few hours.
//
// This is also where Cloud CDN, Cloud Armor (WAF / rate limiting) or IAP
// would be attached later.
func setupLoadBalancer(ctx *pulumi.Context, s *shared, api *cloudrunv2.Service) error {
	afterAPIs := pulumi.DependsOn(s.apis)

	ip, err := compute.NewGlobalAddress(ctx, "api-ip", &compute.GlobalAddressArgs{
		Description: pulumi.Sprintf("Public IP for %s", s.customDomain),
		IpVersion:   pulumi.String("IPV4"),
		AddressType: pulumi.String("EXTERNAL"),
	}, afterAPIs)
	if err != nil {
		return err
	}

	neg, err := compute.NewRegionNetworkEndpointGroup(ctx, "api-neg", &compute.RegionNetworkEndpointGroupArgs{
		Region:              pulumi.String(s.region),
		NetworkEndpointType: pulumi.String("SERVERLESS"),
		CloudRun: &compute.RegionNetworkEndpointGroupCloudRunArgs{
			Service: api.Name,
		},
	}, afterAPIs)
	if err != nil {
		return err
	}

	backend, err := compute.NewBackendService(ctx, "api-backend", &compute.BackendServiceArgs{
		LoadBalancingScheme: pulumi.String("EXTERNAL_MANAGED"),
		Protocol:            pulumi.String("HTTPS"),
		Backends: compute.BackendServiceBackendArray{
			&compute.BackendServiceBackendArgs{Group: neg.ID()},
		},
		LogConfig: &compute.BackendServiceLogConfigArgs{
			Enable:     pulumi.Bool(true),
			SampleRate: pulumi.Float64(1.0),
		},
	})
	if err != nil {
		return err
	}

	urlMap, err := compute.NewURLMap(ctx, "api-https", &compute.URLMapArgs{
		DefaultService: backend.ID(),
	})
	if err != nil {
		return err
	}

	cert, err := compute.NewManagedSslCertificate(ctx, "api-cert", &compute.ManagedSslCertificateArgs{
		Managed: &compute.ManagedSslCertificateManagedArgs{
			Domains: pulumi.StringArray{pulumi.String(s.customDomain)},
		},
	}, afterAPIs)
	if err != nil {
		return err
	}

	httpsProxy, err := compute.NewTargetHttpsProxy(ctx, "api-https", &compute.TargetHttpsProxyArgs{
		UrlMap:          urlMap.ID(),
		SslCertificates: pulumi.StringArray{cert.ID()},
	})
	if err != nil {
		return err
	}
	if _, err := compute.NewGlobalForwardingRule(ctx, "api-https", &compute.GlobalForwardingRuleArgs{
		IpAddress:           ip.Address,
		PortRange:           pulumi.String("443"),
		Target:              httpsProxy.ID(),
		LoadBalancingScheme: pulumi.String("EXTERNAL_MANAGED"),
	}); err != nil {
		return err
	}

	// Plain HTTP only redirects to HTTPS.
	redirect, err := compute.NewURLMap(ctx, "api-http-redirect", &compute.URLMapArgs{
		DefaultUrlRedirect: &compute.URLMapDefaultUrlRedirectArgs{
			HttpsRedirect:        pulumi.Bool(true),
			StripQuery:           pulumi.Bool(false),
			RedirectResponseCode: pulumi.String("MOVED_PERMANENTLY_DEFAULT"),
		},
	}, afterAPIs)
	if err != nil {
		return err
	}
	httpProxy, err := compute.NewTargetHttpProxy(ctx, "api-http", &compute.TargetHttpProxyArgs{
		UrlMap: redirect.ID(),
	})
	if err != nil {
		return err
	}
	if _, err := compute.NewGlobalForwardingRule(ctx, "api-http", &compute.GlobalForwardingRuleArgs{
		IpAddress:           ip.Address,
		PortRange:           pulumi.String("80"),
		Target:              httpProxy.ID(),
		LoadBalancingScheme: pulumi.String("EXTERNAL_MANAGED"),
	}); err != nil {
		return err
	}

	ctx.Export("loadBalancerIp", ip.Address)
	ctx.Export("customDomainDns", pulumi.Sprintf("%s  A  %s", s.customDomain, ip.Address))
	ctx.Export("certificateName", cert.Name) // gcloud compute ssl-certificates describe <name> --global
	return nil
}
