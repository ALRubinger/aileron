import { describe, it, expect, vi, beforeEach } from 'vitest';
import { VERIFICATION_CONSTANT } from './constants';

vi.mock('./argon2.js', () => ({
	deriveKEK: vi.fn()
}));

vi.mock('./envelope.js', () => ({
	decrypt: vi.fn(),
	encrypt: vi.fn()
}));

vi.mock('$lib/api', () => ({
	getPassphraseSalt: vi.fn(),
	getPassphraseVerification: vi.fn(),
	unlockVaultDirect: vi.fn(),
	setPassphrase: vi.fn()
}));

import { unlockVault } from './vault.svelte';
import { deriveKEK } from './argon2.js';
import { decrypt } from './envelope.js';
import { getPassphraseSalt, getPassphraseVerification, unlockVaultDirect } from '$lib/api';

function b64(bytes: Uint8Array): string {
	let binary = '';
	for (const byte of bytes) {
		binary += String.fromCharCode(byte);
	}
	return btoa(binary);
}

describe('unlockVault', () => {
	const kek = new Uint8Array([1, 2, 3, 4]);

	beforeEach(() => {
		vi.clearAllMocks();
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
		vi.mocked(unlockVaultDirect).mockResolvedValue({
			expires_at: '2026-04-29T12:00:00Z'
		});
	});

	it('derives the KEK and sends it directly to the server session cache', async () => {
		const result = await unlockVault('passphrase');

		expect(result.valid).toBe(true);
		expect(result.sessionExpiresAt).toEqual(new Date('2026-04-29T12:00:00Z'));
		// The base64-encoded KEK is transmitted, the passphrase is not.
		// (Captured before unlockVault zeroes the KEK buffer in its finally block.)
		expect(unlockVaultDirect).toHaveBeenCalledWith(b64(new Uint8Array([1, 2, 3, 4])));
	});

	it('reports each unlock phase via the progress callback', async () => {
		const steps: string[] = [];
		await unlockVault('passphrase', (s) => steps.push(s));

		expect(steps).toEqual(['deriving', 'verifying', 'establishing', 'done']);
	});

	it('returns valid:false when the passphrase is wrong (decrypt fails)', async () => {
		vi.mocked(decrypt).mockRejectedValue(new Error('bad tag'));

		const result = await unlockVault('wrong-passphrase');

		expect(result.valid).toBe(false);
		expect(unlockVaultDirect).not.toHaveBeenCalled();
	});

	it('returns valid:false when the verification constant does not match', async () => {
		vi.mocked(decrypt).mockResolvedValue(new TextEncoder().encode('not-the-constant'));

		const result = await unlockVault('wrong-passphrase');

		expect(result.valid).toBe(false);
		expect(unlockVaultDirect).not.toHaveBeenCalled();
	});

	it('throws when no passphrase is set for the account', async () => {
		vi.mocked(getPassphraseSalt).mockResolvedValue({
			has_passphrase: false,
			salt: ''
		});

		await expect(unlockVault('passphrase')).rejects.toThrow(
			'No passphrase set for this account'
		);
		expect(unlockVaultDirect).not.toHaveBeenCalled();
	});
});
