/**
 * Reactive vault lock/unlock state.
 *
 * When a 423 Locked response is received, `requestUnlock` is called with a
 * retry function. The PassphraseModal observes `isVaultLocked()` and opens
 * automatically. After successful verification, `onUnlocked()` retries the
 * pending operation.
 */

let _vaultLocked = $state(false);
let _pendingRetry: (() => Promise<unknown>) | null = $state(null);
let _sessionExpiresAt: Date | null = $state(null);

export function isVaultLocked(): boolean {
	return _vaultLocked;
}

export function sessionExpiresAt(): Date | null {
	return _sessionExpiresAt;
}

export function setSessionExpiresAt(date: Date | null) {
	_sessionExpiresAt = date;
}

/**
 * Called by apiFetch when a 423 response is received. Opens the passphrase
 * modal and returns a promise that resolves with the retry result after
 * the user unlocks.
 */
export function requestUnlock(retryFn: () => Promise<unknown>): Promise<unknown> {
	return new Promise((resolve, reject) => {
		_pendingRetry = async () => {
			try {
				resolve(await retryFn());
			} catch (err) {
				reject(err);
			}
		};
		_vaultLocked = true;
	});
}

/**
 * Called by PassphraseModal after successful passphrase verification.
 * Retries the pending operation and closes the modal.
 */
export async function onUnlocked() {
	_vaultLocked = false;
	if (_pendingRetry) {
		const retry = _pendingRetry;
		_pendingRetry = null;
		await retry();
	}
}

export function dismissUnlock() {
	_vaultLocked = false;
	_pendingRetry = null;
}
