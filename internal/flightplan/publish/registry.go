package publish

import (
	"fmt"
	"strings"

	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// newRemoteRepository builds an oras remote repository for ref (registry +
// repository, any tag/digest ignored), authenticated with the operator's
// existing Docker/OCI credentials. This is the only place publish touches the
// registry, so cmd/aileron never imports oras: `publish.Run` constructs both
// the destination and the foreign-base source repositories here from the ref
// string and the docker credential store.
func newRemoteRepository(ref string) (*remote.Repository, error) {
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

func isLoopbackRegistry(host string) bool {
	h := host
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
