package inject

import (
	"fmt"
	"regexp"
	"strings"
)

// awsRegionPattern matches an AWS region token as it appears as a single
// dotted label in an endpoint host: a two-letter area, one or more
// hyphen-separated alphabetic direction/partition words, and a numeric
// suffix. It matches the commercial regions (us-east-1, eu-west-2,
// ap-southeast-1) and the GovCloud partitions (us-gov-west-1,
// us-gov-east-1), which the earlier two-word-only pattern missed. The
// shape is deliberately conservative so a service label ("athena"), a
// bucket name, or "amazonaws"/"com" is never mistaken for a region.
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}-([a-z]+-)+[0-9]+$`)

// endpointInfixModifiers are non-service labels that can sit between the
// service label and the region label in an AWS endpoint host (e.g. the
// "dualstack" in "s3.dualstack.us-west-2.amazonaws.com"). They are skipped
// when walking back from the region to the service so the SigV4 service
// name is the real service ("s3"), never the modifier.
var endpointInfixModifiers = map[string]bool{
	"dualstack": true,
}

// ParseAWSEndpointHost derives the AWS SigV4 credential scope
// (service, region) from an outbound endpoint host such as
// "athena.us-east-1.amazonaws.com" -> ("athena", "us-east-1"). Any port is
// stripped and the host is lowercased before parsing.
//
// The algorithm scans the dot-separated labels for the first region-shaped
// label ([awsRegionPattern]); the region is that label and the service is
// the nearest preceding label that is not an endpoint modifier. Modifier
// labels ([endpointInfixModifiers], e.g. "dualstack") are skipped, and a
// trailing "-fips" is stripped from the service label, so
// "s3.dualstack.us-west-2.amazonaws.com" and
// "s3-fips.us-east-1.amazonaws.com" both resolve service "s3" rather than
// signing against a modifier-corrupted scope. It does not require a
// ".amazonaws.com" suffix, so test endpoints like
// "athena.us-east-1.amazonaws.test" parse the same way as production hosts.
//
// It fails with [ErrUnparseableAWSHost] when the host is empty, carries no
// region-shaped label (e.g. the legacy global "s3.amazonaws.com" or a
// non-AWS host), or has a region-shaped leading label with no service
// label before it. The host string is echoed into the error to name the
// cause; it is a hostname and never carries credential bytes.
//
// This is the single host->scope parser shared by the SigV4 injector
// (host-authoritative signing scope) and the WASM host's region-scoped
// binding selection, so the two cannot drift.
func ParseAWSEndpointHost(host string) (service, region string, err error) {
	h := strings.TrimSpace(strings.ToLower(host))
	// Strip a trailing :port if present. A hostname never contains a colon,
	// so a single trailing colon-group is a port to drop.
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	if h == "" {
		return "", "", fmt.Errorf("%w: empty host", ErrUnparseableAWSHost)
	}

	labels := strings.Split(h, ".")
	for i, label := range labels {
		if !awsRegionPattern.MatchString(label) {
			continue
		}
		// Walk back past any endpoint modifier labels (e.g. "dualstack") to
		// the real service label.
		j := i - 1
		for j > 0 && endpointInfixModifiers[labels[j]] {
			j--
		}
		if j < 0 || endpointInfixModifiers[labels[j]] {
			return "", "", fmt.Errorf(
				"%w: %q has a region-shaped label with no service label",
				ErrUnparseableAWSHost, host)
		}
		// A FIPS endpoint carries the marker as a "-fips" suffix on the
		// service label (e.g. "s3-fips"); the SigV4 service name is the base
		// service, so strip it.
		return strings.TrimSuffix(labels[j], "-fips"), label, nil
	}
	return "", "", fmt.Errorf("%w: %q has no region-shaped label", ErrUnparseableAWSHost, host)
}
