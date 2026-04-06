package model

import "time"

// UserKeyMaterial stores the non-secret cryptographic material needed to
// verify a user's passphrase and re-derive their Key Encryption Key (KEK).
//
// The KEK itself is NEVER stored server-side. Only the Argon2id salt (for
// re-derivation) and a verification blob (a known constant encrypted with
// the KEK) are persisted. This enables passphrase verification without
// revealing the KEK.
type UserKeyMaterial struct {
	UserID          string    // owning user (usr_ + UUID)
	Salt            []byte    // Argon2id salt for KEK derivation
	KEKVerification []byte    // known constant encrypted with KEK — used to verify passphrase
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
