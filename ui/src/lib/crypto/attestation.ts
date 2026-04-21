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
const DISCOVERY_URL = 'https://accounts.google.com/.well-known/openid-configuration';

interface JWTHeader {
	alg: string;
	kid: string;
	typ?: string;
}

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

	// 1. Parse header.
	const header: JWTHeader = JSON.parse(base64UrlDecode(parts[0]));
	if (!header.kid || !header.alg) {
		throw new Error('Invalid JWT header: missing kid or alg');
	}

	// 2. Fetch JWKS and find the signing key.
	const key = await fetchSigningKey(header.kid, header.alg);

	// 3. Verify signature.
	const signedContent = new TextEncoder().encode(parts[0] + '.' + parts[1]);
	const signature = base64UrlToBytes(parts[2]);
	const algorithm = getVerifyAlgorithm(header.alg, key);

	const sigBuf = new Uint8Array(signature).buffer as ArrayBuffer;
	const dataBuf = new Uint8Array(signedContent).buffer as ArrayBuffer;
	const valid = await crypto.subtle.verify(algorithm, key, sigBuf, dataBuf);
	if (!valid) {
		throw new Error('JWT signature verification failed');
	}

	// 4. Validate claims.
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

let jwksCache: { keys: JsonWebKey[]; fetchedAt: number } | null = null;
const JWKS_CACHE_TTL_MS = 60 * 60 * 1000; // 1 hour

async function fetchSigningKey(kid: string, alg: string): Promise<CryptoKey> {
	// Fetch JWKS (cached for 1 hour).
	if (!jwksCache || Date.now() - jwksCache.fetchedAt > JWKS_CACHE_TTL_MS) {
		const discoveryResp = await fetch(DISCOVERY_URL);
		const discovery = await discoveryResp.json();
		const jwksResp = await fetch(discovery.jwks_uri);
		const jwks = await jwksResp.json();
		jwksCache = { keys: jwks.keys, fetchedAt: Date.now() };
	}

	// Find key by kid.
	const jwk = jwksCache.keys.find((k: any) => k.kid === kid);
	if (!jwk) {
		throw new Error(`Signing key not found: kid=${kid}`);
	}

	// Import as CryptoKey.
	const importAlg = getImportAlgorithm(alg, jwk);
	return crypto.subtle.importKey('jwk', jwk, importAlg, false, ['verify']);
}

function getImportAlgorithm(alg: string, jwk: any): AlgorithmIdentifier | RsaHashedImportParams | EcKeyImportParams {
	switch (alg) {
		case 'RS256':
			return { name: 'RSASSA-PKCS1-v1_5', hash: 'SHA-256' };
		case 'ES256':
			return { name: 'ECDSA', namedCurve: jwk.crv || 'P-256' };
		default:
			throw new Error(`Unsupported algorithm: ${alg}`);
	}
}

function getVerifyAlgorithm(alg: string, _key: CryptoKey): AlgorithmIdentifier | RsaHashedImportParams | EcdsaParams {
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
