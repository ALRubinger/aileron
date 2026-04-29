/**
 * Vault unlock and setup orchestrators.
 *
 * These functions coordinate the client-side crypto flow: KEK derivation,
 * passphrase verification, attestation, ECDH key exchange, and encrypted
 * KEK transmission to the enclave.
 */

import { deriveKEK } from './argon2.js';
import { encrypt, decrypt } from './envelope.js';
import { generateKeyPair, deriveSharedSecret } from './ecdh.js';
import { verifyAttestation } from './attestation.js';
import { VERIFICATION_CONSTANT, SALT_LENGTH } from './constants.js';
import {
	getPassphraseSalt,
	getPassphraseVerification,
	getTeeStatus,
	initiateAttestation,
	establishTeeSession,
	unlockVaultDirect,
	setPassphrase
} from '$lib/api';

export type UnlockProgress =
	| 'deriving'
	| 'verifying'
	| 'attesting'
	| 'establishing'
	| 'done';

export interface UnlockResult {
	valid: boolean;
	sessionExpiresAt?: Date;
	escrowedCount?: number;
}

/**
 * Unlocks the vault by deriving the KEK client-side and verifying the
 * passphrase locally.
 *
 * If TEE is enabled: transmits KEK via end-to-end encrypted channel (ECDH).
 * If TEE is disabled: sends KEK directly over HTTPS to the server's
 * session cache. The passphrase never leaves the browser in either mode.
 */
export async function unlockVault(
	passphrase: string,
	onProgress?: (step: UnlockProgress) => void
): Promise<UnlockResult> {
	// 1. Fetch salt.
	onProgress?.('deriving');
	const saltResp = await getPassphraseSalt();
	if (!saltResp.has_passphrase || !saltResp.salt) {
		throw new Error('No passphrase set for this account');
	}
	const salt = base64ToBytes(saltResp.salt);

	// 2. Derive KEK locally (Argon2id in WASM).
	const kek = await deriveKEK(passphrase, salt);

	try {
		// 3. Fetch verification blob.
		onProgress?.('verifying');
		const verifyResp = await getPassphraseVerification();
		if (!verifyResp.has_passphrase || !verifyResp.kek_verification) {
			throw new Error('No passphrase set for this account');
		}
		const verificationBlob = base64ToBytes(verifyResp.kek_verification);

		// 4. Verify locally: decrypt blob with KEK, check constant.
		let plaintext: Uint8Array;
		try {
			plaintext = await decrypt(verificationBlob, kek);
		} catch {
			return { valid: false };
		}

		const decoded = new TextDecoder().decode(plaintext);
		if (decoded !== VERIFICATION_CONSTANT) {
			return { valid: false };
		}

		// 5. Check if TEE is available.
		const teeStatus = await getTeeStatus();

		if (teeStatus.enabled) {
			// TEE mode: attestation + ECDH + encrypted KEK transmission.
			return await unlockViaTee(kek, onProgress, teeStatus.expected_identity);
		}

		// Direct mode: send KEK over HTTPS to server session cache.
		onProgress?.('establishing');
		const resp = await unlockVaultDirect(bytesToBase64(kek));

		onProgress?.('done');
		return {
			valid: true,
			sessionExpiresAt: resp.expires_at ? new Date(resp.expires_at) : undefined
		};
	} finally {
		kek.fill(0);
	}
}

/**
 * TEE unlock path: attestation, ECDH key exchange, encrypted KEK transmission.
 */
async function unlockViaTee(
	kek: Uint8Array,
	onProgress?: (step: UnlockProgress) => void,
	expectedIdentity?: { image_digest?: string; project_id?: string }
): Promise<UnlockResult> {
	onProgress?.('attesting');
	const attestResp = await initiateAttestation();
	const enclavePublicKey = base64ToBytes(attestResp.public_key);

	const nonce = attestResp.nonce ? base64ToBytes(attestResp.nonce) : undefined;
	const attResult = await verifyAttestation(
		attestResp.token,
		enclavePublicKey,
		'aileron-enclave',
		nonce,
		expectedIdentity
	);
	if (!attResult.verified) {
		throw new Error('Enclave attestation verification failed');
	}

	onProgress?.('establishing');
	const { privateKey, publicKey: clientPubKey } = await generateKeyPair();
	const sharedSecret = await deriveSharedSecret(privateKey, attResult.enclavePublicKey);
	const encryptedKEK = await encrypt(kek, sharedSecret);

	const sessionResp = await establishTeeSession({
		encrypted_kek: bytesToBase64(encryptedKEK),
		client_public_key: bytesToBase64(clientPubKey)
	});

	onProgress?.('done');
	return {
		valid: true,
		sessionExpiresAt: sessionResp.expires_at ? new Date(sessionResp.expires_at) : undefined,
		escrowedCount: sessionResp.escrowed_count
	};
}

/**
 * Sets up a new vault passphrase. The client generates the salt, derives
 * the KEK, encrypts the verification constant, and sends only the salt
 * and encrypted blob to the server. The passphrase and KEK never leave
 * the browser.
 */
export async function setupPassphrase(passphrase: string): Promise<void> {
	// 1. Generate salt.
	const salt = crypto.getRandomValues(new Uint8Array(SALT_LENGTH));

	// 2. Derive KEK locally.
	const kek = await deriveKEK(passphrase, salt);

	try {
		// 3. Encrypt verification constant.
		const verification = await encrypt(
			new TextEncoder().encode(VERIFICATION_CONSTANT),
			kek
		);

		// 4. Send salt + verification blob to server (NOT passphrase, NOT KEK).
		await setPassphrase({
			salt: bytesToBase64(salt),
			kek_verification: bytesToBase64(verification)
		});
	} finally {
		kek.fill(0);
	}
}

// --- Base64 helpers ---

function base64ToBytes(b64: string): Uint8Array {
	const binary = atob(b64);
	const bytes = new Uint8Array(binary.length);
	for (let i = 0; i < binary.length; i++) {
		bytes[i] = binary.charCodeAt(i);
	}
	return bytes;
}

function bytesToBase64(bytes: Uint8Array): string {
	let binary = '';
	for (let i = 0; i < bytes.length; i++) {
		binary += String.fromCharCode(bytes[i]);
	}
	return btoa(binary);
}
