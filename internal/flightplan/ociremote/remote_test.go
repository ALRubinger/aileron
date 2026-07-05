package ociremote

import "testing"

func TestNewRepositoryLoopbackUsesPlainHTTP(t *testing.T) {
	repo, err := NewRepository("localhost:5000/acme/plan")
	if err != nil {
		t.Fatalf("localhost repo: %v", err)
	}
	if !repo.PlainHTTP {
		t.Error("localhost registry should use PlainHTTP")
	}
}

func TestNewRepositoryRemoteUsesHTTPS(t *testing.T) {
	repo, err := NewRepository("ghcr.io/acme/plan:v1")
	if err != nil {
		t.Fatalf("ghcr repo: %v", err)
	}
	if repo.PlainHTTP {
		t.Error("ghcr.io registry should use HTTPS, not PlainHTTP")
	}
}

func TestNewRepositoryInvalidRefErrors(t *testing.T) {
	if _, err := NewRepository("not a ref!!"); err == nil {
		t.Error("invalid reference: want error")
	}
}

func TestIsLoopbackRegistry(t *testing.T) {
	cases := map[string]bool{
		"localhost:5000":           true,
		"127.0.0.1:5000":           true,
		"localhost":                true,
		"127.0.0.1":                true,
		"ghcr.io":                  false,
		"docker.io":                false,
		"registry.example.com:443": false,
	}
	for host, want := range cases {
		if got := isLoopbackRegistry(host); got != want {
			t.Errorf("isLoopbackRegistry(%q) = %v, want %v", host, got, want)
		}
	}
}
