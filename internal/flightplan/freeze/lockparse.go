package freeze

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ParseLockfile parses standalone `aileron.lock` bytes (the artifact file
// MarshalLockfile produces) back into a Lockfile. It is the read counterpart
// of MarshalLockfile: the lockfile is YAML, so a consumer that has only the
// standalone artifact bytes (e.g. `skill publish` reading the frozen version's
// lock to find the image pin) parses them here rather than re-deriving an
// unexported path.
//
// This reads the lock without verifying the signature; callers that need the
// verified lock use the runtime load path. `skill publish` is a pass-through
// push (it re-uploads the already-signed bytes and the consumer verifies at
// install/launch), so parsing the unverified lock only to locate the image pin
// to push is sufficient.
func ParseLockfile(data []byte) (Lockfile, error) {
	var l Lockfile
	if err := yaml.Unmarshal(data, &l); err != nil {
		return Lockfile{}, fmt.Errorf("freeze: parse lockfile: %w", err)
	}
	return l, nil
}
