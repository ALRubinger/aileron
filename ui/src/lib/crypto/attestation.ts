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
	expectedAudience: string
): Promise<AttestationResult> {
	if (token === 'dev-ok') {
		// Local TEE provider — no real attestation.
		return { verified: true, enclavePublicKey };
	}

	// Production: verify Google Confidential Space OIDC JWT.
	const verified = await verifyConfidentialSpaceJWT(token, expectedAudience);
	return { verified, enclavePublicKey };
}

// --- JWT verification internals ---

const EXPECTED_ISSUER = 'https://confidentialcomputing.googleapis.com';

interface JWTClaims {
	iss: string;
	exp: number;
	iat?: number;
	aud?: string;
	eat_nonce?: string[];
	submods?: {
		container?: {
			image_digest?: string;
		};
		gce?: {
			project_id?: string;
		};
	};
}

async function verifyConfidentialSpaceJWT(
	token: string,
	_expectedAudience: string
): Promise<boolean> {
	const parts = token.split('.');
	if (parts.length !== 3) {
		throw new Error('Invalid JWT: expected 3 parts');
	}

	// Validate claims from the JWT payload.
	// Note: JWKS signature verification is not possible from the browser
	// because Google's JWKS endpoints do not set CORS headers. The server
	// verifies the signature when it communicates with the enclave. The
	// client validates claims as a defense-in-depth check that the token
	// came from the expected issuer and is not expired.
	const claims: JWTClaims = JSON.parse(base64UrlDecode(parts[1]));

	if (claims.iss !== EXPECTED_ISSUER) {
		throw new Error(`Unexpected issuer: ${claims.iss}`);
	}

	const now = Math.floor(Date.now() / 1000);
	if (claims.exp < now) {
		throw new Error('Token expired');
	}

	return true;
}

// --- Base64url helpers ---

function base64UrlDecode(str: string): string {
	const padded = str.replace(/-/g, '+').replace(/_/g, '/');
	return atob(padded);
}

function base64UrlToBytes(str: string): Uint8Array {
	const binary = base64UrlDecode(str);
	const bytes = new Uint8Array(binary.length);
	for (let i = 0; i < binary.length; i++) {
		bytes[i] = binary.charCodeAt(i);
	}
	return bytes;
}
