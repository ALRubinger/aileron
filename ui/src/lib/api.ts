import { PUBLIC_API_BASE } from '$env/static/public';
import { getToken, refreshAuth, clearAuth } from '$lib/auth.svelte.js';
import { requestUnlock } from '$lib/vault.svelte.js';

const API_BASE = PUBLIC_API_BASE;

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function apiFetch(path: string, options?: RequestInit): Promise<any> {
	const headers: Record<string, string> = {
		'Content-Type': 'application/json',
		...Object.fromEntries(new Headers(options?.headers).entries())
	};

	const token = getToken();
	if (token && token !== 'cookie-auth') {
		headers['Authorization'] = `Bearer ${token}`;
	}

	let res = await fetch(`${API_BASE}${path}`, { ...options, headers, credentials: 'include' });

	// If unauthorized, attempt token refresh and retry once.
	if (res.status === 401 && token) {
		const refreshed = await refreshAuth();
		if (refreshed) {
			const newToken = getToken();
			if (newToken) {
				headers['Authorization'] = `Bearer ${newToken}`;
			}
			res = await fetch(`${API_BASE}${path}`, { ...options, headers, credentials: 'include' });
		} else {
			clearAuth();
			if (typeof window !== 'undefined') {
				window.location.href = '/login';
			}
			throw new Error('Session expired');
		}
	}

	if (res.status === 204) return null;

	// Vault locked — open passphrase modal and retry.
	if (res.status === 423) {
		return requestUnlock(() => apiFetch(path, options));
	}

	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: { message: res.statusText } }));
		throw new Error(err.error?.message || res.statusText);
	}
	return res.json();
}

export async function listApprovals(workspaceId = 'default') {
	return apiFetch(`/v1/approvals?workspace_id=${workspaceId}`);
}

export async function getApproval(approvalId: string) {
	return apiFetch(`/v1/approvals/${approvalId}`);
}

export async function approveRequest(approvalId: string, comment?: string) {
	return apiFetch(`/v1/approvals/${approvalId}/approve`, {
		method: 'POST',
		body: JSON.stringify({ comment })
	});
}

export async function denyRequest(approvalId: string, reason: string, comment?: string) {
	return apiFetch(`/v1/approvals/${approvalId}/deny`, {
		method: 'POST',
		body: JSON.stringify({ reason, comment })
	});
}

// --- Action-level approvals (#418) ---
//
// Distinct from the rich governance flow above (`/v1/approvals` with
// intents and multi-approver workflows). The action-approval queue is
// the runtime-blocking shape: one entry per held-open
// `POST /v1/actions/{name}/run` waiting for a single yes/no decision.
// Per the launch path, the agent surfaces the approval URL to the
// user via templated MCP tool descriptions; the user decides here.

export type PendingActionApproval = {
	id: string;
	action_name: string;
	connector_fqn?: string;
	args?: Record<string, unknown>;
	session_id?: string;
	requested_at: string;
};

export type ActionApprovalListResponse = {
	items: PendingActionApproval[];
};

/** Returns the queue of pending action-level approvals. The webapp polls
 *  this every few seconds while open; the backend is in-memory per-process,
 *  so an empty list after the daemon restarts is normal (#412 will give
 *  this persistence later). */
export async function listActionApprovals(): Promise<ActionApprovalListResponse> {
	return apiFetch('/v1/action-approvals');
}

/** Payload of the `resolved` SSE event. Carries the user's verdict so the
 *  webapp can drop the matching pending card without re-fetching the list. */
export type ResolvedActionApproval = {
	id: string;
	approved: boolean;
	reason?: string;
	decided_at: string;
};

/** Callbacks the SSE subscriber invokes on each event class. The page
 *  passes handlers that update its local state; the SSE wiring concerns
 *  itself only with parsing frames and reconnect. */
export type ActionApprovalSubscriber = {
	onSnapshot: (items: PendingActionApproval[]) => void;
	onPending: (item: PendingActionApproval) => void;
	onResolved: (resolved: ResolvedActionApproval) => void;
	onError?: (err: unknown) => void;
};

/** Opens an SSE connection to `/v1/action-approvals/watch` and forwards
 *  events to the supplied subscriber. Returns a close function the
 *  caller MUST invoke on unmount; calling it tears down the EventSource
 *  and stops dispatch.
 *
 *  Reconnect is delegated to the browser's built-in EventSource retry
 *  semantics (default 3s, configurable server-side via `retry:` lines).
 *  The page's existing fallback `listActionApprovals()` poll on the
 *  governance side covers the (rare) case where the SSE stream has
 *  been broken long enough to drop events. */
export function watchActionApprovals(sub: ActionApprovalSubscriber): () => void {
	const url = `${API_BASE}/v1/action-approvals/watch`;
	const es = new EventSource(url, { withCredentials: true });

	es.addEventListener('snapshot', (e: MessageEvent) => {
		try {
			const payload = JSON.parse(e.data);
			sub.onSnapshot(payload.items ?? []);
		} catch (err) {
			sub.onError?.(err);
		}
	});
	es.addEventListener('pending', (e: MessageEvent) => {
		try {
			sub.onPending(JSON.parse(e.data));
		} catch (err) {
			sub.onError?.(err);
		}
	});
	es.addEventListener('resolved', (e: MessageEvent) => {
		try {
			sub.onResolved(JSON.parse(e.data));
		} catch (err) {
			sub.onError?.(err);
		}
	});
	es.addEventListener('error', (e) => {
		// EventSource auto-reconnects on transient failure. We only
		// surface the error once per disconnect — readyState === CLOSED
		// means the browser gave up; CONNECTING means it'll retry.
		if (es.readyState === EventSource.CLOSED) {
			sub.onError?.(e);
		}
	});

	return () => es.close();
}

/** Resolves a pending action-approval. The runtime's blocked
 *  `RunAction` unblocks on the next tick; on `approved=true` the action
 *  proceeds normally, on `approved=false` it returns an
 *  `approval_denied` failure envelope to the agent with `reason` in the
 *  message body. Returns null on success (server replies 200 with empty
 *  body); throws on 404 (already resolved) or other non-2xx. */
export async function decideActionApproval(
	approvalId: string,
	approved: boolean,
	reason?: string
): Promise<null> {
	return apiFetch(`/v1/action-approvals/${approvalId}/decide`, {
		method: 'POST',
		body: JSON.stringify({ approved, reason: reason ?? '' })
	});
}

export async function getIntent(intentId: string) {
	return apiFetch(`/v1/intents/${intentId}`);
}

export async function listTraces(workspaceId = 'default') {
	return apiFetch(`/v1/traces?workspace_id=${workspaceId}`);
}

export async function listPolicies(workspaceId = 'default') {
	return apiFetch(`/v1/policies?workspace_id=${workspaceId}`);
}

// --- Connected Accounts ---

export async function listConnectedAccounts() {
	return apiFetch('/v1/connected-accounts');
}

export async function getConnectedAccount(id: string) {
	return apiFetch(`/v1/connected-accounts/${id}`);
}

export async function deleteConnectedAccount(id: string) {
	return apiFetch(`/v1/connected-accounts/${id}`, { method: 'DELETE' });
}

export async function getCurrentUser() {
	return apiFetch('/v1/users/me');
}

export async function disconnectAuthProvider(provider: string) {
	return apiFetch(`/v1/users/me/auth-providers/${encodeURIComponent(provider)}`, {
		method: 'DELETE'
	});
}

export async function updateCurrentUser(data: { display_name?: string }) {
	return apiFetch('/v1/users/me', {
		method: 'PATCH',
		body: JSON.stringify(data)
	});
}

export async function getCurrentEnterprise() {
	return apiFetch('/v1/enterprises/me');
}

export async function updateCurrentEnterprise(data: {
	name?: string;
	billing_email?: string;
	sso_required?: boolean;
	allowed_auth_providers?: string[];
	allowed_email_domains?: string[];
}) {
	return apiFetch('/v1/enterprises/me', {
		method: 'PATCH',
		body: JSON.stringify(data)
	});
}

// --- LLM Provider Configuration ---

export async function getUserLLMConfig() {
	return apiFetch('/v1/llm-config');
}

export async function upsertUserLLMConfig(data: {
	provider: string;
	model_research: string;
	model_synthesis: string;
	api_key: string;
}) {
	return apiFetch('/v1/llm-config', {
		method: 'PUT',
		body: JSON.stringify(data)
	});
}

export async function deleteUserLLMConfig() {
	return apiFetch('/v1/llm-config', { method: 'DELETE' });
}

export async function getEnterpriseLLMConfig(enterpriseId: string) {
	return apiFetch(`/v1/enterprises/${enterpriseId}/llm-config`);
}

export async function upsertEnterpriseLLMConfig(
	enterpriseId: string,
	data: {
		provider: string;
		model_research: string;
		model_synthesis: string;
		api_key: string;
	}
) {
	return apiFetch(`/v1/enterprises/${enterpriseId}/llm-config`, {
		method: 'PUT',
		body: JSON.stringify(data)
	});
}

export async function deleteEnterpriseLLMConfig(enterpriseId: string) {
	return apiFetch(`/v1/enterprises/${enterpriseId}/llm-config`, { method: 'DELETE' });
}

// --- Vault / Passphrase ---

export async function getPassphraseSalt() {
	return apiFetch('/v1/users/me/passphrase/salt');
}

export async function getPassphraseVerification() {
	return apiFetch('/v1/users/me/passphrase/verification');
}

export async function setPassphrase(data: { salt: string; kek_verification: string }) {
	return apiFetch('/v1/users/me/passphrase', {
		method: 'POST',
		body: JSON.stringify(data)
	});
}

export async function getVaultStatus() {
	return apiFetch('/v1/users/me/vault/status');
}

export async function lockVault() {
	return apiFetch('/v1/users/me/passphrase/lock', { method: 'POST' });
}

export async function unlockVaultDirect(kek: string) {
	return apiFetch('/v1/users/me/passphrase/unlock', {
		method: 'POST',
		body: JSON.stringify({ kek })
	});
}

// --- TEE ---

export async function initiateAttestation(audience?: string) {
	return apiFetch('/v1/tee/attestation', {
		method: 'POST',
		body: JSON.stringify({ audience })
	});
}

export async function establishTeeSession(data: {
	encrypted_kek: string;
	client_public_key: string;
}) {
	return apiFetch('/v1/tee/session', {
		method: 'POST',
		body: JSON.stringify(data)
	});
}

export async function getTeeStatus() {
	return apiFetch('/v1/tee/status');
}
