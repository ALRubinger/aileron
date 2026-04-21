import { PUBLIC_API_BASE } from '$env/static/public';

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

const EXPECTED_ISSUER = 'https://accounts.google.com';

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

interface JWK {
	kty: string;
	kid: string;
	alg?: string;
	use?: string;
	n?: string;
	e?: string;
	crv?: string;
	x?: string;
	y?: string;
}

interface JWKS {
	keys: JWK[];
}

async function verifyConfidentialSpaceJWT(
	token: string,
	_expectedAudience: string
): Promise<boolean> {
	const parts = token.split('.');
	if (parts.length !== 3) {
		throw new Error('Invalid JWT: expected 3 parts');
	}

	// Decode header to extract key ID.
	const header: { alg: string; kid: string } = JSON.parse(base64UrlDecode(parts[0]));

	// Validate claims from the JWT payload.
	const claims: JWTClaims = JSON.parse(base64UrlDecode(parts[1]));

	if (claims.iss !== EXPECTED_ISSUER) {
		throw new Error(`Unexpected issuer: ${claims.iss}`);
	}

	const now = Math.floor(Date.now() / 1000);
	if (claims.exp < now) {
		throw new Error('Token expired');
	}

	// Verify JWT signature using JWKS fetched from our proxy endpoint.
	const jwks = await fetchJWKS();
	const jwk = jwks.keys.find((k) => k.kid === header.kid);
	if (!jwk) {
		throw new Error(`Signing key ${header.kid} not found in JWKS`);
	}

	const signingInput = new TextEncoder().encode(parts[0] + '.' + parts[1]);
	const signature = base64UrlToBytes(parts[2]);
	const key = await importKey(jwk, header.alg);

	const algorithm = getVerifyAlgorithm(header.alg);
	const valid = await crypto.subtle.verify(
		algorithm,
		key,
		(signature as Uint8Array<ArrayBuffer>).buffer,
		(signingInput as Uint8Array<ArrayBuffer>).buffer
	);
	if (!valid) {
		throw new Error('JWT signature verification failed');
	}

	return true;
}

/**
 * Fetches the JWKS from the API server's proxy endpoint.
 * The server caches upstream responses for 1 hour.
 */
async function fetchJWKS(): Promise<JWKS> {
	const resp = await fetch(`${PUBLIC_API_BASE}/v1/tee/jwks`);
	if (!resp.ok) {
		throw new Error(`Failed to fetch JWKS: ${resp.status} ${resp.statusText}`);
	}
	return resp.json();
}

/**
 * Imports a JWK as a CryptoKey for signature verification.
 */
async function importKey(jwk: JWK, alg: string): Promise<CryptoKey> {
	const algorithm = getImportAlgorithm(alg);
	return crypto.subtle.importKey('jwk', jwk as JsonWebKey, algorithm, false, ['verify']);
}

function getImportAlgorithm(alg: string): RsaHashedImportParams | EcKeyImportParams {
	switch (alg) {
		case 'RS256':
			return { name: 'RSASSA-PKCS1-v1_5', hash: 'SHA-256' };
		case 'ES256':
			return { name: 'ECDSA', namedCurve: 'P-256' };
		default:
			throw new Error(`Unsupported algorithm: ${alg}`);
	}
}

function getVerifyAlgorithm(alg: string): AlgorithmIdentifier | RsaPssParams | EcdsaParams {
	switch (alg) {
		case 'RS256':
			return { name: 'RSASSA-PKCS1-v1_5' };
		case 'ES256':
			return { name: 'ECDSA', hash: 'SHA-256' };
		default:
			throw new Error(`Unsupported algorithm: ${alg}`);
	}
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
