import { PUBLIC_API_BASE } from '$env/static/public';

const TOKEN_KEY = 'aileron_access_token';
const REFRESH_KEY = 'aileron_refresh_token';

/** Reactive auth state using Svelte 5 runes via module-level $state. */
let _token = $state<string | null>(null);
let _user = $state<AuthUser | null>(null);

export interface AuthUser {
	id: string;
	email: string;
	display_name: string;
	avatar_url?: string;
	role: string;
	enterprise_id: string;
}

/** Initialize auth state from localStorage (call once on app mount). */
export function initAuth() {
	if (typeof window === 'undefined') return;
	_token = localStorage.getItem(TOKEN_KEY);
	if (_token) {
		fetchCurrentUser().catch(() => clearAuth());
	}
}

/** Returns the current access token, or null. */
export function getToken(): string | null {
	return _token;
}

/** Returns the current user, or null. */
export function getUser(): AuthUser | null {
	return _user;
}

/** Returns true if the user is authenticated. */
export function isAuthenticated(): boolean {
	return _token !== null;
}

/** Store tokens after login/signup and fetch user profile. */
export async function setAuth(accessToken: string, refreshToken?: string) {
	_token = accessToken;
	localStorage.setItem(TOKEN_KEY, accessToken);
	if (refreshToken) {
		localStorage.setItem(REFRESH_KEY, refreshToken);
	}
	await fetchCurrentUser();
}

/** Clear auth state (logout). */
export function clearAuth() {
	_token = null;
	_user = null;
	localStorage.removeItem(TOKEN_KEY);
	localStorage.removeItem(REFRESH_KEY);
}

/** Attempt to refresh the access token using the stored refresh token. */
export async function refreshAuth(): Promise<boolean> {
	const refreshToken = localStorage.getItem(REFRESH_KEY);
	if (!refreshToken) return false;

	try {
		const res = await fetch(`${PUBLIC_API_BASE}/auth/refresh`, {
			method: 'POST',
			credentials: 'include',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ refresh_token: refreshToken })
		});
		if (!res.ok) {
			clearAuth();
			return false;
		}
		const data = await res.json();
		_token = data.access_token;
		localStorage.setItem(TOKEN_KEY, data.access_token);
		return true;
	} catch {
		clearAuth();
		return false;
	}
}

/** Re-fetch the current user profile and update reactive state. */
export async function refreshUser() {
	return fetchCurrentUser();
}

/** Fetch the current user profile from /v1/users/me. */
async function fetchCurrentUser() {
	const headers: Record<string, string> = {};
	// Use Bearer token for password-based auth; use cookies for OAuth flow.
	if (_token && _token !== 'cookie-auth') {
		headers['Authorization'] = `Bearer ${_token}`;
	}
	const res = await fetch(`${PUBLIC_API_BASE}/v1/users/me`, {
		headers,
		credentials: 'include'
	});
	if (!res.ok) throw new Error('Failed to fetch user');
	_user = await res.json();
}

// --- Auth API calls (signup, login, verify) ---

export async function signup(email: string, password: string, displayName?: string) {
	const res = await fetch(`${PUBLIC_API_BASE}/auth/signup`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ email, password, display_name: displayName })
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
	return res.json();
}

export async function verifyEmail(email: string, code: string) {
	const res = await fetch(`${PUBLIC_API_BASE}/auth/verify-email`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ email, code })
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
	return res.json();
}

export async function emailLogin(email: string, password: string) {
	const res = await fetch(`${PUBLIC_API_BASE}/auth/login`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ email, password })
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error || res.statusText);
	}
	return res.json();
}

export async function logout() {
	try {
		await fetch(`${PUBLIC_API_BASE}/auth/logout`, {
			method: 'POST',
			headers: _token ? { Authorization: `Bearer ${_token}` } : {}
		});
	} catch {
		// Best-effort server-side logout.
	}
	clearAuth();
}
