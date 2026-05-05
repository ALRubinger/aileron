// Vault-state store. Drives the passphrase modal opened by the
// layout when the daemon reports the local vault as locked (or when
// any /v1/* call comes back with 423).
//
// Mirrors the pattern in `ui/src/lib/vault.svelte.ts` (the cloud-tier
// reference): a request that's blocked on vault-locked queues its
// retry; on unlock, the modal calls onUnlocked() and the queued
// retries resume. Single-tab focus, single user — no cross-tab
// coordination is needed.

import { getLocalVaultStatus, type LocalVaultStatus } from './api';

let _locked = $state(false);
let _state: LocalVaultStatus['state'] = $state('unlocked');
let _pending: Array<() => Promise<unknown>> = [];
let _resolvers: Array<(v: unknown) => void> = [];
let _rejecters: Array<(e: unknown) => void> = [];

/** Read-only flag the layout reads to drive the modal's open state. */
export function isVaultLocked(): boolean {
	return _locked;
}

/** Latest known state classification, surfaced in the lock-state
 *  chip (`unlocked` / `locked` / `missing`). */
export function vaultState(): LocalVaultStatus['state'] {
	return _state;
}

/** Polls the daemon and updates the local state. Called on app load
 *  from the layout so the modal opens proactively when the daemon
 *  starts vault-locked, without waiting for the first 423. */
export async function refreshVaultStatus(): Promise<void> {
	try {
		const status = await getLocalVaultStatus();
		_state = status.state;
		_locked = status.locked;
	} catch {
		// Best-effort; if the status endpoint isn't reachable, leave
		// the state alone. The 423-driven path will still trigger the
		// modal when needed.
	}
}

/** Called by apiFetch when a request returns 423. Opens the modal,
 *  queues the retry, and returns a promise that resolves when the
 *  retry succeeds (or rejects if the user cancels). */
export function onVaultLocked<T>(retry: () => Promise<T>): Promise<T> {
	_locked = true;
	if (_state === 'unlocked') _state = 'locked';
	return new Promise<T>((resolve, reject) => {
		_pending.push(retry as () => Promise<unknown>);
		_resolvers.push(resolve as (v: unknown) => void);
		_rejecters.push(reject);
	});
}

/** Called by the modal after a successful unlock. Drains the queued
 *  retries; each runs in order, with its result/error fed back to
 *  the caller that originally got the 423. */
export async function onVaultUnlocked(): Promise<void> {
	_locked = false;
	_state = 'unlocked';
	const pending = _pending;
	const resolvers = _resolvers;
	const rejecters = _rejecters;
	_pending = [];
	_resolvers = [];
	_rejecters = [];
	for (let i = 0; i < pending.length; i++) {
		try {
			const result = await pending[i]();
			resolvers[i](result);
		} catch (err) {
			rejecters[i](err);
		}
	}
}

/** Called by the modal if the user dismisses without unlocking. The
 *  queued callers are rejected so they don't hang. */
export function onVaultDismissed(): void {
	const rejecters = _rejecters;
	_pending = [];
	_resolvers = [];
	_rejecters = [];
	for (const reject of rejecters) {
		reject(new Error('vault unlock cancelled'));
	}
}
