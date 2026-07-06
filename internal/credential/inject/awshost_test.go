package inject

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAWSEndpointHostVectors(t *testing.T) {
	cases := []struct {
		name        string
		host        string
		wantService string
		wantRegion  string
	}{
		{"standard", "athena.us-east-1.amazonaws.com", "athena", "us-east-1"},
		{"eu-west", "athena.eu-west-2.amazonaws.com", "athena", "eu-west-2"},
		{"ap-southeast", "s3.ap-southeast-1.amazonaws.com", "s3", "ap-southeast-1"},
		{"govcloud-west", "athena.us-gov-west-1.amazonaws.com", "athena", "us-gov-west-1"},
		{"govcloud-east", "s3.us-gov-east-1.amazonaws.com", "s3", "us-gov-east-1"},
		{"test-tld", "athena.us-east-1.amazonaws.test", "athena", "us-east-1"},
		{"with-port", "athena.us-east-1.amazonaws.com:443", "athena", "us-east-1"},
		{"uppercase", "Athena.US-East-1.amazonaws.com", "athena", "us-east-1"},
		{"dualstack", "s3.dualstack.us-west-2.amazonaws.com", "s3", "us-west-2"},
		{"fips", "s3-fips.us-east-1.amazonaws.com", "s3", "us-east-1"},
		{"fips-dualstack", "s3-fips.dualstack.us-east-1.amazonaws.com", "s3", "us-east-1"},
		{"api-ecr", "api.ecr.us-east-1.amazonaws.com", "ecr", "us-east-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, region, err := ParseAWSEndpointHost(tc.host)
			if err != nil {
				t.Fatalf("ParseAWSEndpointHost(%q) unexpected error: %v", tc.host, err)
			}
			if service != tc.wantService {
				t.Errorf("service = %q, want %q", service, tc.wantService)
			}
			if region != tc.wantRegion {
				t.Errorf("region = %q, want %q", region, tc.wantRegion)
			}
		})
	}
}

func TestParseAWSEndpointHostErrors(t *testing.T) {
	cases := []struct {
		name string
		host string
	}{
		{"empty", ""},
		{"port-only-empty", ":443"},
		{"legacy-global", "example.amazonaws.com"},
		{"s3-global", "s3.amazonaws.com"},
		{"non-aws", "api.example.com"},
		{"service-only", "athena.amazonaws.com"},
		{"region-leading-no-service", "us-east-1.amazonaws.com"},
		{"modifier-only-no-service", "dualstack.us-west-2.amazonaws.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, region, err := ParseAWSEndpointHost(tc.host)
			if !errors.Is(err, ErrUnparseableAWSHost) {
				t.Fatalf("ParseAWSEndpointHost(%q) error = %v, want ErrUnparseableAWSHost", tc.host, err)
			}
			if service != "" || region != "" {
				t.Errorf("on error want empty (service,region), got (%q,%q)", service, region)
			}
		})
	}
}

// TestParseAWSEndpointHostServiceIsHostLabel pins the host-label == signing
// service assumption that ParseAWSEndpointHost relies on. For every service
// Aileron signs today the SigV4 service name equals the endpoint host label,
// so the parser returns that label verbatim as the service. Athena is the
// live example (athena.<region> -> service "athena"); this test fails loudly
// if the derived service ever stops matching the host label for it.
func TestParseAWSEndpointHostServiceIsHostLabel(t *testing.T) {
	const region = "us-east-1"
	cases := []struct {
		hostLabel string // the leading (service-shaped) label of the endpoint host
	}{
		{"athena"},
		{"s3"},
	}
	for _, tc := range cases {
		t.Run(tc.hostLabel, func(t *testing.T) {
			host := tc.hostLabel + "." + region + ".amazonaws.com"
			service, gotRegion, err := ParseAWSEndpointHost(host)
			if err != nil {
				t.Fatalf("ParseAWSEndpointHost(%q) unexpected error: %v", host, err)
			}
			if service != tc.hostLabel {
				t.Errorf("service = %q, want host label %q (host-label==service assumption)", service, tc.hostLabel)
			}
			if gotRegion != region {
				t.Errorf("region = %q, want %q", gotRegion, region)
			}
		})
	}
}

// TestParseAWSEndpointHostDivergentServiceDeferred documents the known cliff
// in the host-label == signing service assumption: a few AWS services must be
// signed under a SigV4 service name that differs from their endpoint host
// label. These are NOT on Aileron's signing path today, and divergent-service
// handling is deferred (see ParseAWSEndpointHost's doc comment). This test
// pins the CURRENT behavior — the parser returns the raw host label, not the
// real signing service — so that if a mapping is ever added, the follow-up is
// forced to update this test deliberately. The wantSigningService field
// records the service these hosts would actually need to sign as; it is the
// deferred cliff, intentionally NOT what the parser returns today.
func TestParseAWSEndpointHostDivergentServiceDeferred(t *testing.T) {
	const region = "us-east-1"
	cases := []struct {
		name string
		host string
		// wantLabel is what ParseAWSEndpointHost returns today (the host
		// label), which is what we assert on.
		wantLabel string
		// wantSigningService is the correct SigV4 service for this endpoint.
		// It differs from wantLabel; handling this divergence is DEFERRED, so
		// it is documented here but deliberately not asserted as the result.
		wantSigningService string
	}{
		{"ses-email", "email." + region + ".amazonaws.com", "email", "ses"},
		{"simpledb-sdb", "sdb." + region + ".amazonaws.com", "sdb", "sdb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, gotRegion, err := ParseAWSEndpointHost(tc.host)
			if err != nil {
				t.Fatalf("ParseAWSEndpointHost(%q) unexpected error: %v", tc.host, err)
			}
			if service != tc.wantLabel {
				t.Errorf("service = %q, want host label %q (divergent-service handling is deferred; real signing service would be %q)",
					service, tc.wantLabel, tc.wantSigningService)
			}
			if gotRegion != region {
				t.Errorf("region = %q, want %q", gotRegion, region)
			}
		})
	}
}

// TestParseAWSEndpointHostErrorNamesHost asserts the error clearly names the
// offending host so an operator can diagnose it. The host is non-secret;
// there is no credential material in this function at all.
func TestParseAWSEndpointHostErrorNamesHost(t *testing.T) {
	const host = "not-a-region.example.com"
	_, _, err := ParseAWSEndpointHost(host)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), host) {
		t.Errorf("error %q does not name the host %q", err.Error(), host)
	}
}
