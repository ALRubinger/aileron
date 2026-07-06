package credreq

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ALRubinger/aileron/internal/credential/inject"
)

// HostShape names how the egress boundary selects a derived binding: by the
// upstream host it reaches, or by the credential identity it carries. It is a
// named type (not a bare bool) so tests assert on the semantic, not on which
// side of a boolean the mapping happens to fall.
type HostShape string

const (
	// HostShapeHostKeyed selects the binding by upstream host: the proxy
	// matches the request's host against the binding's host pattern before
	// injecting. This is the shape for header-carried credentials (bearer),
	// whose scope is the set of hosts the step declares.
	HostShapeHostKeyed HostShape = "host-keyed"

	// HostShapeHostLessIdentity selects the binding by its (kind,
	// identityLabel) pair, not by host (#1978). The AWS SigV4 re-sign scheme
	// derives its credential scope from the resolved upstream host at egress,
	// so the descriptor needs no host to select the binding. Hosts declared on
	// such a contract are carried for audit context but are not the selection
	// key.
	HostShapeHostLessIdentity HostShape = "host-less-identity"
)

// schemeMapping is one row of the closed kind -> (scheme, host-shape) table.
// skip marks the unauthenticated `none` kind, which yields no requirement.
type schemeMapping struct {
	scheme string
	shape  HostShape
	skip   bool
}

// kindSchemeTable is the single, closed mapping from a trust contract's
// credential.kind to the injection scheme and host-shape a derived binding
// carries. It fails closed: a kind absent from this table is an error, never a
// guessed scheme.
//
//   - aws-sigv4 -> sigv4-resign, host-less-identity: the load-bearing case
//     (#1978); the descriptor is selected by identity, the host is derived at
//     egress and not needed to select the binding.
//   - oauth2 -> bearer, host-keyed: the schema pins an oauth2 credential to
//     placement=header, which is the bearer wire form.
//   - api-key -> bearer, host-keyed: a DELIBERATE, documented default for the
//     header-bearer wire form. It is a default for a MAPPED kind, not a guess
//     on an unknown one. A query-param or header-template api-key would need
//     placement-aware selection, which is out of scope here (see the package
//     doc); adding it requires plumbing placement onto runtime.TrustContract.
//   - none -> skip: an unauthenticated step declares no credential to onboard.
var kindSchemeTable = map[string]schemeMapping{
	"aws-sigv4": {scheme: string(inject.SchemeSigV4Resign), shape: HostShapeHostLessIdentity},
	"oauth2":    {scheme: string(inject.SchemeBearer), shape: HostShapeHostKeyed},
	"api-key":   {scheme: string(inject.SchemeBearer), shape: HostShapeHostKeyed},
	"none":      {skip: true},
}

// mappedKinds returns the table's keys sorted, for a deterministic error
// message that names the closed set.
func mappedKinds() []string {
	ks := make([]string, 0, len(kindSchemeTable))
	for k := range kindSchemeTable {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// schemeFor maps a credential.kind to its injection scheme and host-shape. It
// fails closed: a kind outside the closed table returns an error naming the
// offending kind and the mapped set, so an unknown kind never yields a guessed
// scheme. The `none` kind returns skip=true (an unauthenticated step onboards
// no credential); its scheme and shape are meaningless and must not be read.
func schemeFor(kind string) (scheme string, shape HostShape, skip bool, err error) {
	m, ok := kindSchemeTable[kind]
	if !ok {
		return "", "", false, fmt.Errorf(
			"credreq: unmapped credential kind %q (mapped kinds: %s)",
			kind, strings.Join(mappedKinds(), ", "))
	}
	return m.scheme, m.shape, m.skip, nil
}
