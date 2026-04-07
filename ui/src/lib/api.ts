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
