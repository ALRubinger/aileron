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
