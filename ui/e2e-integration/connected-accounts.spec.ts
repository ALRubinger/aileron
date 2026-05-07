import { test, expect, API_URL } from './auth-fixture';

test.describe('Connected Accounts — Real API', () => {
	test('API returns empty accounts list for fresh user', async ({ request }) => {
		const token = await (await import('./auth-fixture')).ensureAuth();
		const resp = await request.get(`${API_URL}/v1/connected-accounts`, {
			headers: { Authorization: `Bearer ${token}` }
		});

		expect(resp.ok()).toBe(true);
		const body = await resp.json();
		expect(body.items).toEqual([]);
	});

	test('API returns vault status for authenticated user', async ({ request }) => {
		const token = await (await import('./auth-fixture')).ensureAuth();
		const resp = await request.get(`${API_URL}/v1/users/me/vault/status`, {
			headers: { Authorization: `Bearer ${token}` }
		});

		expect(resp.ok()).toBe(true);
		const body = await resp.json();
		// Fresh user has no passphrase set
		expect(body).toHaveProperty('has_passphrase');
		expect(body).toHaveProperty('locked');
	});

	test('available integrations shown or redirects to vault setup', async ({ authedPage: page }) => {
		await page.goto('/settings/connected-accounts');

		// Wait for a state signal that lands AFTER the GET /v1/connected-accounts
		// fetch resolves — either the vault-setup redirect copy or the
		// landed empty-state copy. The Connected Accounts card title
		// renders before the fetch resolves, so gating on the title
		// raced against the empty state and the providers list (#501).
		const settled = page
			.getByText('Secure your vault')
			.or(page.getByText('No accounts connected yet.'));
		await expect(settled).toBeVisible({ timeout: 15000 });

		if (page.url().includes('/vault')) {
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

		// Empty state already verified by the `settled` matcher above —
		// reaching this point means the fetch resolved and the page has
		// rendered its post-fetch content. The provider list shares the
		// same fetch, so the next assertions land deterministically.
		await expect(page.getByText('Slack', { exact: true })).toBeVisible();
		await expect(page.getByText('GitHub')).toBeVisible();
		await expect(page.getByText('Gmail', { exact: true })).toBeVisible();
		await expect(page.getByText('Google Calendar')).toBeVisible();
	});
});
