const NONCE_LENGTH = 12;

/** Convert Uint8Array to a plain ArrayBuffer (avoids SharedArrayBuffer issues). */
function toBuffer(arr: Uint8Array): ArrayBuffer {
	return arr.buffer instanceof ArrayBuffer
		? arr.buffer.slice(arr.byteOffset, arr.byteOffset + arr.byteLength)
		: arr.slice().buffer;
}

/**
 * Encrypts plaintext with AES-256-GCM. Returns nonce || ciphertext || tag.
 * Wire format matches Go's core/crypto/envelope.go.
 */
export async function encrypt(plaintext: Uint8Array, key: Uint8Array): Promise<Uint8Array> {
	const nonce = crypto.getRandomValues(new Uint8Array(NONCE_LENGTH));
	const cryptoKey = await crypto.subtle.importKey('raw', toBuffer(key), 'AES-GCM', false, [
		'encrypt'
	]);
	const ciphertext = await crypto.subtle.encrypt(
		{ name: 'AES-GCM', iv: nonce },
		cryptoKey,
		toBuffer(plaintext)
	);
	// Concatenate: nonce || ciphertext+tag
	const result = new Uint8Array(NONCE_LENGTH + ciphertext.byteLength);
	result.set(nonce, 0);
	result.set(new Uint8Array(ciphertext), NONCE_LENGTH);
	return result;
}

/**
 * Decrypts data produced by encrypt(). Expects nonce || ciphertext || tag.
 */
export async function decrypt(data: Uint8Array, key: Uint8Array): Promise<Uint8Array> {
	if (data.length < NONCE_LENGTH) {
		throw new Error('ciphertext too short');
	}
	const nonce = data.slice(0, NONCE_LENGTH);
	const ciphertext = data.slice(NONCE_LENGTH);
	const cryptoKey = await crypto.subtle.importKey('raw', toBuffer(key), 'AES-GCM', false, [
		'decrypt'
	]);
	const plaintext = await crypto.subtle.decrypt(
		{ name: 'AES-GCM', iv: nonce },
		cryptoKey,
		toBuffer(ciphertext)
	);
	return new Uint8Array(plaintext);
}
