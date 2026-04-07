export interface AttestationResult {
	verified: boolean;
	enclavePublicKey: Uint8Array;
}

/**
 * Verifies an enclave attestation token and extracts the enclave's ECDH
 * public key. In dev mode (token === "dev-ok"), verification is skipped.
 * In production, verifies the OIDC JWT against Google's JWKS.
 */
export async function verifyAttestation(
	token: string,
	enclavePublicKey: Uint8Array,
	_expectedAudience: string
): Promise<AttestationResult> {
	if (token === 'dev-ok') {
		// Local TEE provider — no real attestation. The public key was
		// returned alongside the token from the attestation endpoint.
		return { verified: true, enclavePublicKey };
	}

	// Production path: verify OIDC JWT from Confidential Space.
	// TODO(#56): Implement full JWT verification against Google JWKS:
	// 1. Parse JWT header to get kid
	// 2. Fetch JWKS from https://www.googleapis.com/oauth2/v3/certs
	// 3. Verify signature using Web Crypto subtle.verify()
	// 4. Validate claims: iss, exp, aud, image_digest, project_id
	// For now, reject non-dev tokens until production TEE is deployed.
	throw new Error('Production attestation verification not yet implemented');
}
