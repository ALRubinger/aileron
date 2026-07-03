package app

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
)

// TestSandboxForwardProxy_SigV4ResignAgainstRealAWS drives the daemon's real
// upstream leg (default http.Client — real Transport, real ALPN/h2 negotiation
// with AWS) end to end against the real Athena endpoint, with the request
// entering through the same CONNECT/MITM proxy path a rung-3 tool uses. It is
// the one test that lets AWS itself judge the re-signed signature — the
// local wire test can't exercise HTTP/2 header serialization to AWS.
//
// Gated on env so CI never needs credentials:
//
//	AILERON_SIGV4_LIVE_AKID / AILERON_SIGV4_LIVE_SECRET  — a real (throwaway,
//	read-only) key pair; AILERON_SIGV4_LIVE_REGION defaults us-east-1.
func TestSandboxForwardProxy_SigV4ResignAgainstRealAWS(t *testing.T) {
	akid := os.Getenv("AILERON_SIGV4_LIVE_AKID")
	secret := os.Getenv("AILERON_SIGV4_LIVE_SECRET")
	if akid == "" || secret == "" {
		t.Skip("AILERON_SIGV4_LIVE_AKID/SECRET not set; skipping live AWS signature test")
	}
	region := os.Getenv("AILERON_SIGV4_LIVE_REGION")
	if region == "" {
		region = "us-east-1"
	}
	host := "athena." + region + ".amazonaws.com"

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-sigv4-live" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte(secret)),
		hostBindings: mustHostBindingTableOpts(t, host, "user/aws", "sigv4-resign",
			binding.WithSigV4Resign(akid, region, "athena")),
		// The REAL upstream leg: default client, real Transport, real AWS.
		sandboxProxyClient: &http.Client{},
	}
	setup := newHostBindingProxySetup(t, srv)

	body := `{}`
	req, err := http.NewRequest(http.MethodPost, "https://"+host+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonAthena.ListWorkGroups")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "aws-cli/2.35.15 md/awscrt#0.35.0 ua/2.1 os/linux#6.12.76-linuxkit md/arch#aarch64 lang/python#3.14.5")
	req.Header.Set("Amz-Sdk-Invocation-Id", "45b9a48b-67da-4d67-b1b3-a0347fe6f6a7")
	req.Header.Set("Amz-Sdk-Request", "attempt=1")
	req.Header.Set("X-Amz-Date", "20260703T000000Z")
	req.Header.Set("X-Amz-Content-Sha256", "placeholder")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7PLACEHLDR/20260703/"+region+"/athena/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")

	resp, err := setup.client.Do(req)
	if err != nil {
		t.Fatalf("POST through proxy to real AWS: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("real AWS returned %d:\n%s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "WorkGroups") {
		t.Fatalf("unexpected ListWorkGroups body: %s", respBody)
	}
	t.Logf("real AWS accepted the re-signed request: %d, %d bytes", resp.StatusCode, len(respBody))
}
