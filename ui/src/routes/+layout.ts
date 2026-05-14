import { redirect, isRedirect } from '@sveltejs/kit';
import { browser } from '$app/environment';
import { initAuth, isAuthenticated } from '$lib/auth.svelte.js';
import { getVaultStatus } from '$lib/api';

const PUBLIC_PATHS = ['/login', '/signup', '/verify-email', '/auth/callback'];

export const load = async ({ url }) => {
	if (!browser) return;

	const path = url.pathname;
	const isPublic = PUBLIC_PATHS.some((p) => path.startsWith(p));

	initAuth();

	if (!isPublic && !isAuthenticated()) {
		const params = path === '/' ? '' : `?redirectTo=${encodeURIComponent(path)}`;
		throw redirect(303, `/login${params}`);
	}

	if (isPublic || !isAuthenticated() || path === '/vault') return;

	try {
		const status = await getVaultStatus();
		if (!status.has_passphrase) {
			throw redirect(303, '/vault');
		}
	} catch (err) {
		if (isRedirect(err)) throw err;
	}
};
