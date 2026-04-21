import { describe, it, expect, vi, beforeEach } from 'vitest';
import { verifyAttestation } from './attestation';

// Helper: base64url encode without padding.
function base64UrlEncode(data: Uint8Array): string {
	const binary = String.fromCharCode(...data);
	return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function base64UrlEncodeString(str: string): string {
	return btoa(str).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// Creates a valid signed JWT using Web Crypto.
async function createSignedJWT(
	claims: Record<string, unknown>,
	keyPair: CryptoKeyPair,
	kid: string,
	alg: string = 'RS256'
): Promise<string> {
	const header = base64UrlEncodeString(JSON.stringify({ alg, kid, typ: 'JWT' }));
	const payload = base64UrlEncodeString(JSON.stringify(claims));
	const signingInput = new TextEncoder().encode(`${header}.${payload}`);

	const algorithm =
		alg === 'RS256' ? { name: 'RSASSA-PKCS1-v1_5' } : { name: 'ECDSA', hash: 'SHA-256' };
	const sig = await crypto.subtle.sign(algorithm, keyPair.privateKey, signingInput);
	const signature = base64UrlEncode(new Uint8Array(sig));

	return `${header}.${payload}.${signature}`;
}

async function exportJWK(key: CryptoKey, kid: string): Promise<JsonWebKey & { kid: string }> {
	const jwk = await crypto.subtle.exportKey('jwk', key);
	return { ...jwk, kid };
}

describe('verifyAttestation', () => {
	const enclavePublicKey = new Uint8Array([1, 2, 3, 4]);

	it('returns verified=true for dev-ok token', async () => {
		const result = await verifyAttestation('dev-ok', enclavePublicKey, 'test-audience');
		expect(result.verified).toBe(true);
		expect(result.enclavePublicKey).toBe(enclavePublicKey);
	});

	it('throws on invalid JWT format', async () => {
		await expect(verifyAttestation('not-a-jwt', enclavePublicKey, 'aud')).rejects.toThrow(
			'Invalid JWT: expected 3 parts'
		);
	});

	describe('with RSA-signed JWT', () => {
		let keyPair: CryptoKeyPair;
		const kid = 'test-kid-rsa';

		beforeEach(async () => {
			keyPair = await crypto.subtle.generateKey(
				{ name: 'RSASSA-PKCS1-v1_5', modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: 'SHA-256' },
				true,
				['sign', 'verify']
			);

			const jwk = await exportJWK(keyPair.publicKey, kid);
			vi.spyOn(globalThis, 'fetch').mockResolvedValue(
				new Response(JSON.stringify({ keys: [jwk] }), {
					status: 200,
					headers: { 'Content-Type': 'application/json' }
				})
			);
		});

		it('verifies a valid RS256 JWT', async () => {
			const claims = {
				iss: 'https://accounts.google.com',
				exp: Math.floor(Date.now() / 1000) + 3600
			};
			const token = await createSignedJWT(claims, keyPair, kid, 'RS256');

			const result = await verifyAttestation(token, enclavePublicKey, 'aud');
			expect(result.verified).toBe(true);
		});

		it('throws on wrong issuer', async () => {
			const claims = {
				iss: 'https://evil.example.com',
				exp: Math.floor(Date.now() / 1000) + 3600
			};
			const token = await createSignedJWT(claims, keyPair, kid, 'RS256');

			await expect(verifyAttestation(token, enclavePublicKey, 'aud')).rejects.toThrow(
				'Unexpected issuer'
			);
		});

		it('throws on expired token', async () => {
			const claims = {
				iss: 'https://accounts.google.com',
				exp: Math.floor(Date.now() / 1000) - 60
			};
			const token = await createSignedJWT(claims, keyPair, kid, 'RS256');

			await expect(verifyAttestation(token, enclavePublicKey, 'aud')).rejects.toThrow(
				'Token expired'
			);
		});

		it('throws when signing key is not in JWKS', async () => {
			vi.spyOn(globalThis, 'fetch').mockResolvedValue(
				new Response(JSON.stringify({ keys: [] }), {
					status: 200,
					headers: { 'Content-Type': 'application/json' }
				})
			);

			const claims = {
				iss: 'https://accounts.google.com',
				exp: Math.floor(Date.now() / 1000) + 3600
			};
			const token = await createSignedJWT(claims, keyPair, kid, 'RS256');

			await expect(verifyAttestation(token, enclavePublicKey, 'aud')).rejects.toThrow(
				'not found in JWKS'
			);
		});

		it('throws on tampered payload', async () => {
			const claims = {
				iss: 'https://accounts.google.com',
				exp: Math.floor(Date.now() / 1000) + 3600
			};
			const token = await createSignedJWT(claims, keyPair, kid, 'RS256');

			// Tamper with the payload.
			const parts = token.split('.');
			const tamperedClaims = { ...claims, iss: 'https://accounts.google.com', extra: 'tampered' };
			parts[1] = base64UrlEncodeString(JSON.stringify(tamperedClaims));
			const tampered = parts.join('.');

			await expect(verifyAttestation(tampered, enclavePublicKey, 'aud')).rejects.toThrow(
				'signature verification failed'
			);
		});
	});

	describe('with ECDSA-signed JWT', () => {
		let keyPair: CryptoKeyPair;
		const kid = 'test-kid-ec';

		beforeEach(async () => {
			keyPair = await crypto.subtle.generateKey(
				{ name: 'ECDSA', namedCurve: 'P-256' },
				true,
				['sign', 'verify']
			);

			const jwk = await exportJWK(keyPair.publicKey, kid);
			vi.spyOn(globalThis, 'fetch').mockResolvedValue(
				new Response(JSON.stringify({ keys: [jwk] }), {
					status: 200,
					headers: { 'Content-Type': 'application/json' }
				})
			);
		});

		it('verifies a valid ES256 JWT', async () => {
			const claims = {
				iss: 'https://accounts.google.com',
				exp: Math.floor(Date.now() / 1000) + 3600
			};
			const token = await createSignedJWT(claims, keyPair, kid, 'ES256');

			const result = await verifyAttestation(token, enclavePublicKey, 'aud');
			expect(result.verified).toBe(true);
		});
	});

	it('throws on unsupported algorithm', async () => {
		vi.spyOn(globalThis, 'fetch').mockResolvedValue(
			new Response(JSON.stringify({ keys: [{ kty: 'OKP', kid: 'ed-key' }] }), {
				status: 200,
				headers: { 'Content-Type': 'application/json' }
			})
		);

		const claims = {
			iss: 'https://accounts.google.com',
			exp: Math.floor(Date.now() / 1000) + 3600
		};
		const header = base64UrlEncodeString(JSON.stringify({ alg: 'EdDSA', kid: 'ed-key' }));
		const payload = base64UrlEncodeString(JSON.stringify(claims));
		const token = `${header}.${payload}.fake-sig`;

		await expect(verifyAttestation(token, enclavePublicKey, 'aud')).rejects.toThrow(
			'Unsupported algorithm'
		);
	});

	it('throws when JWKS fetch fails', async () => {
		vi.spyOn(globalThis, 'fetch').mockResolvedValue(
			new Response('Internal Server Error', { status: 500 })
		);

		const claims = {
			iss: 'https://accounts.google.com',
			exp: Math.floor(Date.now() / 1000) + 3600
		};
		// Create a minimal JWT (signature won't matter since fetch fails before verify).
		const header = base64UrlEncodeString(JSON.stringify({ alg: 'RS256', kid: 'x' }));
		const payload = base64UrlEncodeString(JSON.stringify(claims));
		const token = `${header}.${payload}.fake-sig`;

		await expect(verifyAttestation(token, enclavePublicKey, 'aud')).rejects.toThrow(
			'Failed to fetch JWKS'
		);
	});
});
