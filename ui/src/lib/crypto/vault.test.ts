import { describe, it, expect, vi, beforeEach } from 'vitest';
import { VERIFICATION_CONSTANT } from './constants';

vi.mock('./argon2.js', () => ({
	deriveKEK: vi.fn()
}));

vi.mock('./envelope.js', () => ({
	decrypt: vi.fn(),
	encrypt: vi.fn()
}));

vi.mock('./ecdh.js', () => ({
	generateKeyPair: vi.fn(),
	deriveSharedSecret: vi.fn()
}));

vi.mock('./attestation.js', () => ({
	verifyAttestation: vi.fn()
}));

vi.mock('$lib/api', () => ({
	getPassphraseSalt: vi.fn(),
	getPassphraseVerification: vi.fn(),
	getTeeStatus: vi.fn(),
	initiateAttestation: vi.fn(),
	establishTeeSession: vi.fn(),
	unlockVaultDirect: vi.fn(),
	setPassphrase: vi.fn()
}));

import { unlockVault } from './vault.svelte';
import { deriveKEK } from './argon2.js';
import { decrypt, encrypt } from './envelope.js';
import { generateKeyPair, deriveSharedSecret } from './ecdh.js';
import { verifyAttestation } from './attestation.js';
import {
	getPassphraseSalt,
	getPassphraseVerification,
	getTeeStatus,
	initiateAttestation,
	establishTeeSession
} from '$lib/api';

function b64(bytes: Uint8Array): string {
	let binary = '';
	for (const byte of bytes) {
		binary += String.fromCharCode(byte);
	}
	return btoa(binary);
}

describe('unlockVault', () => {
	const kek = new Uint8Array([1, 2, 3, 4]);
	const expectedIdentity = {
		image_digest: 'sha256:expected',
		project_id: 'expected-project'
	};

	beforeEach(() => {
		vi.mocked(getPassphraseSalt).mockResolvedValue({
			has_passphrase: true,
			salt: b64(new Uint8Array(16).fill(7))
		});
		vi.mocked(deriveKEK).mockResolvedValue(kek);
		vi.mocked(getPassphraseVerification).mockResolvedValue({
			has_passphrase: true,
			kek_verification: b64(new Uint8Array([9, 9, 9]))
		});
		vi.mocked(decrypt).mockResolvedValue(new TextEncoder().encode(VERIFICATION_CONSTANT));
		vi.mocked(getTeeStatus).mockResolvedValue({
			enabled: true,
			provider: 'confidential-space',
			expected_identity: expectedIdentity
		});
		vi.mocked(initiateAttestation).mockResolvedValue({
			token: 'attestation-token',
			public_key: b64(new Uint8Array([5, 6, 7, 8])),
			nonce: b64(new Uint8Array([9, 10]))
		});
		vi.mocked(verifyAttestation).mockResolvedValue({
			verified: true,
			enclavePublicKey: new Uint8Array([5, 6, 7, 8])
		});
		vi.mocked(generateKeyPair).mockResolvedValue({
			privateKey: {} as CryptoKey,
			publicKey: new Uint8Array([11, 12])
		});
		vi.mocked(deriveSharedSecret).mockResolvedValue(new Uint8Array([13, 14]));
		vi.mocked(encrypt).mockResolvedValue(new Uint8Array([15, 16]));
		vi.mocked(establishTeeSession).mockResolvedValue({
			expires_at: '2026-04-29T12:00:00Z',
			escrowed_count: 3
		});
	});

	it('passes expected enclave identity pins into browser attestation verification', async () => {
		const result = await unlockVault('passphrase');

		expect(result.valid).toBe(true);
		expect(result.escrowedCount).toBe(3);
		expect(verifyAttestation).toHaveBeenCalledWith(
			'attestation-token',
			new Uint8Array([5, 6, 7, 8]),
			'aileron-enclave',
			new Uint8Array([9, 10]),
			expectedIdentity
		);
	});
});
