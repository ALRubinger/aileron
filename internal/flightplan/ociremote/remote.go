// Package ociremote builds oras remote repositories from an OCI reference and
// the operator's Docker/OCI credentials. It is the single place the flightplan
// distribution paths touch the registry: both `skill publish` (#1901) and
// `skill install` (#1902) construct their remote targets here, so neither the
// publish nor the pull package duplicates the credential-store + plain-HTTP
// wiring, and cmd/aileron stays oras-free.
package ociremote

import (
	"fmt"
	"net"
	"strings"

	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// NewRepository builds an oras remote repository for ref (registry +
// repository; any tag/digest in ref is ignored), authenticated with the
// operator's existing Docker/OCI credentials. This is the shared registry
// entry point for publish (destination + foreign-base source) and pull
// (install source), so cmd/aileron never imports oras: the callers pass a ref
// string and this constructs the repository from it and the docker credential
// store.
func NewRepository(ref string) (*remote.Repository, error) {
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("parse reference: %w", err)
	}
	repo, err := remote.NewRepository(parsed.Registry + "/" + parsed.Repository)
	if err != nil {
		return nil, err
	}

	credStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("load docker credentials: %w", err)
	}
	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.NewCache(),
		Credential: credentials.Credential(credStore),
	}
	// A localhost registry (the common local/e2e case) is served over plain
	// HTTP; a real remote registry uses HTTPS.
	repo.PlainHTTP = isLoopbackRegistry(parsed.Registry)
	return repo, nil
}

// isLoopbackRegistry reports whether host addresses a loopback registry, which
// is served over plain HTTP rather than HTTPS. It handles a bare host, a
// host:port, and bracketed IPv6 (`[::1]` / `[::1]:5000`): net.SplitHostPort
// strips a port when present, and the surviving brackets are removed before
// comparing, so `localhost`, `127.0.0.1`, and `::1` all resolve correctly with
// or without a port.
func isLoopbackRegistry(host string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
