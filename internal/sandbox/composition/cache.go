package composition

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// CacheVolumePrefix is the common name prefix for every Aileron-managed sandbox
// cache volume. The manual GC command (`aileron sandbox cache clear`) keys off
// this prefix to find the volumes it may remove, so no Aileron-owned cache
// volume may exist without it.
const CacheVolumePrefix = "aileron-cache"

// CacheVolumeName derives the deterministic Docker named-volume name for a cache
// keyed by (cli, identity). The CLI is sanitized into the name for human
// legibility; the identity is hashed (never carried verbatim) so re-auth to a
// different workspace/identity yields a distinct volume and caches never leak
// across identities.
//
// Shape: aileron-cache-<sanitized-cli>-<sha12(identity)>. The same (cli,
// identity) pair always maps to the same name, which is what lets a tool's cache
// survive across launches; a different identity maps to a different name, which
// is what scopes the cache to that identity.
func CacheVolumeName(cli, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	digest := hex.EncodeToString(sum[:])[:12]
	return CacheVolumePrefix + "-" + sanitizeVolumeSegment(cli) + "-" + digest
}

// sanitizeVolumeSegment reduces an arbitrary CLI label to the character set
// Docker accepts in a volume name ([a-zA-Z0-9][a-zA-Z0-9_.-]*). Any other rune
// becomes '-'; an empty result collapses to "cli" so the assembled name is
// always a valid, non-empty volume name. The identity is not run through this
// helper because it is hashed, not embedded.
func sanitizeVolumeSegment(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "cli"
	}
	return out
}
