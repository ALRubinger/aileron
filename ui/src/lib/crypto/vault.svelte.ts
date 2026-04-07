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
	initiateAttestation,
	establishTeeSession,
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
 * Unlocks the vault by deriving the KEK client-side, verifying the
 * passphrase locally, then transmitting the KEK to the enclave via
 * an end-to-end encrypted channel.
 *
 * The passphrase and KEK never leave the browser in plaintext.
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

		// 5. Initiate attestation.
		onProgress?.('attesting');
		const attestResp = await initiateAttestation();
		const enclavePublicKey = base64ToBytes(attestResp.public_key);

		// 6. Verify attestation client-side.
		const attResult = await verifyAttestation(
			attestResp.token,
			enclavePublicKey,
			'aileron-enclave'
		);
		if (!attResult.verified) {
			throw new Error('Enclave attestation verification failed');
		}

		// 7. Generate ephemeral ECDH key pair.
		onProgress?.('establishing');
		const { privateKey, publicKey: clientPubKey } = await generateKeyPair();

		// 8. Derive shared secret using enclave's attested public key.
		const sharedSecret = await deriveSharedSecret(privateKey, attResult.enclavePublicKey);

		// 9. Encrypt KEK with shared secret.
		const encryptedKEK = await encrypt(kek, sharedSecret);

		// 10. Send opaque blob through server to enclave.
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
	} finally {
		// Best-effort memory zeroing (JS doesn't guarantee this, but we try).
		kek.fill(0);
	}
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
