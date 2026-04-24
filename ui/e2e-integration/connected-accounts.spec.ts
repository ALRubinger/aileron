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

		// Wait for the page to settle — either vault setup or connected accounts content
		const settled = page
			.getByText('Secure your vault')
			.or(page.locator('[data-slot="card-title"]', { hasText: 'Connected Accounts' }));
		await expect(settled).toBeVisible({ timeout: 15000 });

		if (page.url().includes('/vault')) {
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

		// Verify empty state
		await expect(page.getByText('No accounts connected yet.')).toBeVisible();

		// All four providers should be listed
		await expect(page.getByText('Slack', { exact: true })).toBeVisible();
		await expect(page.getByText('GitHub')).toBeVisible();
		await expect(page.getByText('Gmail', { exact: true })).toBeVisible();
		await expect(page.getByText('Google Calendar')).toBeVisible();
	});
});
