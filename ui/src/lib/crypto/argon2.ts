import { argon2id } from 'hash-wasm';

/**
 * Derives a 256-bit KEK from a passphrase and salt using Argon2id.
 * Parameters match the server-side constants in core/crypto/kek.go.
 */
export async function deriveKEK(passphrase: string, salt: Uint8Array): Promise<Uint8Array> {
	const hash = await argon2id({
		password: passphrase,
		salt,
		iterations: 3,
		memorySize: 65536, // 64 MB
		parallelism: 4,
		hashLength: 32,
		outputType: 'binary'
	});
	return new Uint8Array(hash);
}
