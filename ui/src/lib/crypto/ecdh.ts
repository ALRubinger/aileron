/**
 * Generates an ephemeral P-256 ECDH key pair.
 * The public key is exported in uncompressed format (65 bytes),
 * matching Go's ecdh.PublicKey.Bytes().
 */
export async function generateKeyPair(): Promise<{ privateKey: CryptoKey; publicKey: Uint8Array }> {
	const keyPair = await crypto.subtle.generateKey(
		{ name: 'ECDH', namedCurve: 'P-256' },
		false, // private key not extractable
		['deriveBits']
	);
	const rawPublic = await crypto.subtle.exportKey('raw', keyPair.publicKey);
	return {
		privateKey: keyPair.privateKey,
		publicKey: new Uint8Array(rawPublic)
	};
}

/**
 * Derives a 256-bit shared secret via ECDH + SHA-256.
 * Matches Go's crypto.DeriveSharedSecret: SHA-256(ECDH raw output).
 */
export async function deriveSharedSecret(
	privateKey: CryptoKey,
	peerPublicKey: Uint8Array
): Promise<Uint8Array> {
	// Import the peer's raw public key.
	const peerBuf =
		peerPublicKey.buffer instanceof ArrayBuffer
			? peerPublicKey.buffer.slice(
					peerPublicKey.byteOffset,
					peerPublicKey.byteOffset + peerPublicKey.byteLength
				)
			: peerPublicKey.slice().buffer;
	const peerKey = await crypto.subtle.importKey(
		'raw',
		peerBuf,
		{ name: 'ECDH', namedCurve: 'P-256' },
		false,
		[]
	);

	// ECDH deriveBits returns the raw x-coordinate (256 bits for P-256).
	const rawBits = await crypto.subtle.deriveBits(
		{ name: 'ECDH', public: peerKey },
		privateKey,
		256
	);

	// Hash with SHA-256 to match Go's implementation.
	const hash = await crypto.subtle.digest('SHA-256', rawBits);
	return new Uint8Array(hash);
}
