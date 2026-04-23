import { test, expect } from './coverage-fixture';

/**
 * E2E tests for the Slack Agent integration UI surfaces.
 *
 * The Slack Agent framework (#240) requires a connected Slack user token
 * (stored in the zero-knowledge vault) so the agent can send messages as
 * the user. These tests verify the Connected Accounts page correctly
 * gates, displays, and manages the Slack connection that backs the agent.
 */

const slackAccount = {
	id: 'conn_slack',
	provider: 'slack',
	external_user_id: 'U04AGENT',
	status: 'active',
	scopes: ['channels:read', 'chat:write', 'users:read', 'im:history'],
	created_at: '2026-04-20T00:00:00Z',
	updated_at: '2026-04-20T00:00:00Z'
};

function mockApi(
	page: import('@playwright/test').Page,
	opts: {
		accounts?: typeof slackAccount[];
		vaultStatus?: { locked: boolean; has_passphrase: boolean };
	} = {}
) {
	const accounts = opts.accounts ?? [slackAccount];
	const vaultStatus = opts.vaultStatus ?? { locked: false, has_passphrase: true };

	return Promise.all([
		page.route('**/v1/connected-accounts', (route) => {
			if (route.request().method() === 'GET') {
				return route.fulfill({ json: { items: accounts } });
			}
			if (route.request().method() === 'DELETE') {
				return route.fulfill({ status: 204 });
			}
			return route.continue();
		}),
		page.route('**/v1/users/me/vault/status', (route) =>
			route.fulfill({ json: vaultStatus })
		),
		page.route('**/v1/users/me', (route) =>
			route.fulfill({
				json: {
					id: 'usr_1',
					email: 'test@example.com',
					display_name: 'Test User',
					role: 'owner',
					status: 'active',
					enterprise_id: 'ent_1'
				}
			})
		)
	]);
}

async function setAuth(page: import('@playwright/test').Page) {
	await page.addInitScript(() => {
		localStorage.setItem('aileron_access_token', 'fake-token-for-e2e');
	});
}

test.describe('Slack Agent — Connected Account', () => {
	test('displays connected Slack account with active status badge', async ({ page }) => {
		await setAuth(page);
		await mockApi(page);

		await page.goto('/settings/connected-accounts');

		// Provider name and user ID visible
		await expect(page.getByText('Slack', { exact: true })).toBeVisible();
		await expect(page.getByText('U04AGENT')).toBeVisible();

		// Active status badge
		const badge = page.locator('[data-slot="badge"]', { hasText: 'active' });
		await expect(badge).toBeVisible();

		// Connection date is displayed
		await expect(page.getByText(/Connected .+ 2026/)).toBeVisible();
	});

	test('shows expired status for a stale Slack connection', async ({ page }) => {
		await setAuth(page);
		await mockApi(page, {
			accounts: [{ ...slackAccount, status: 'expired' }]
		});

		await page.goto('/settings/connected-accounts');

		const badge = page.locator('[data-slot="badge"]', { hasText: 'expired' });
		await expect(badge).toBeVisible();
	});

	test('shows revoked status for a revoked Slack connection', async ({ page }) => {
		await setAuth(page);
		await mockApi(page, {
			accounts: [{ ...slackAccount, status: 'revoked' }]
		});

		await page.goto('/settings/connected-accounts');

		const badge = page.locator('[data-slot="badge"]', { hasText: 'revoked' });
		await expect(badge).toBeVisible();
	});

	test('disconnect flow removes Slack account after confirmation', async ({ page }) => {
		await setAuth(page);

		// After disconnect, the reload returns an empty list
		let disconnected = false;
		await Promise.all([
			page.route('**/v1/connected-accounts/**', (route) => {
				if (route.request().method() === 'DELETE') {
					disconnected = true;
					return route.fulfill({ status: 204 });
				}
				return route.continue();
			}),
			page.route('**/v1/connected-accounts', (route) => {
				if (route.request().method() === 'GET') {
					return route.fulfill({
						json: { items: disconnected ? [] : [slackAccount] }
					});
				}
				return route.continue();
			}),
			page.route('**/v1/users/me/vault/status', (route) =>
				route.fulfill({ json: { locked: false, has_passphrase: true } })
			),
			page.route('**/v1/users/me', (route) =>
				route.fulfill({
					json: {
						id: 'usr_1',
						email: 'test@example.com',
						display_name: 'Test User',
						role: 'owner',
						status: 'active',
						enterprise_id: 'ent_1'
					}
				})
			)
		]);

		await page.goto('/settings/connected-accounts');

		// Click Disconnect
		await page.getByRole('button', { name: 'Disconnect' }).click();

		// Confirm in dialog
		await expect(page.getByText(/Disconnect Slack\?/)).toBeVisible();
		await page.getByRole('button', { name: 'Disconnect' }).nth(1).click();

		// Account removed, Slack now appears in Available Integrations
		await expect(page.getByText('No accounts connected yet.')).toBeVisible();
		await expect(page.getByText('Channels, messages, and search')).toBeVisible();
	});
});

test.describe('Slack Agent — Vault Gating', () => {
	test('connect button disabled when vault is locked', async ({ page }) => {
		await setAuth(page);
		await mockApi(page, {
			accounts: [],
			vaultStatus: { locked: true, has_passphrase: true }
		});

		await page.goto('/settings/connected-accounts');

		// Vault locked card shown
		await expect(page.getByText('Vault locked')).toBeVisible();

		// Slack connect button is disabled
		const connectButton = page
			.locator('div', { hasText: 'Slack' })
			.locator('button', { hasText: 'Connect' });
		await expect(connectButton.first()).toBeDisabled();

		// Guidance text
		await expect(
			page.getByText('Unlock your vault above to connect new accounts.')
		).toBeVisible();
	});

	test('redirects to vault setup when no passphrase exists', async ({ page }) => {
		await setAuth(page);
		await mockApi(page, {
			accounts: [],
			vaultStatus: { locked: true, has_passphrase: false }
		});

		await page.goto('/settings/connected-accounts');

		// App redirects to the vault setup page when no passphrase is configured
		await expect(page).toHaveURL(/\/setup-vault/);
		await expect(page.getByText('Secure your vault')).toBeVisible();
	});

	test('connect button enabled when vault is unlocked', async ({ page }) => {
		await setAuth(page);
		await mockApi(page, {
			accounts: [],
			vaultStatus: { locked: false, has_passphrase: true }
		});

		await page.goto('/settings/connected-accounts');

		// No vault warning cards
		await expect(page.getByText('Vault locked')).not.toBeVisible();
		await expect(page.getByText('Secure your vault')).not.toBeVisible();

		// Slack connect button is enabled
		const connectButton = page
			.locator('div', { hasText: 'Slack' })
			.locator('button', { hasText: 'Connect' });
		await expect(connectButton.first()).toBeEnabled();
	});

	test('connect button navigates to Slack OAuth endpoint', async ({ page }) => {
		await setAuth(page);
		await mockApi(page, {
			accounts: [],
			vaultStatus: { locked: false, has_passphrase: true }
		});

		await page.goto('/settings/connected-accounts');

		// Click Connect on Slack — verify it hits the correct OAuth URL
		const slackCard = page.locator('div.rounded-lg.border', {
			hasText: 'Slack'
		});
		const connectButton = slackCard.getByRole('button', { name: 'Connect' });

		const [request] = await Promise.all([
			page.waitForRequest((req) => req.url().includes('/v1/connect/slack')),
			connectButton.click()
		]);

		expect(request.url()).toContain('/v1/connect/slack');
		expect(request.url()).toContain('return_to=');
	});
});

test.describe('Slack Agent — No Slack Connected', () => {
	test('Slack appears in available integrations with correct description', async ({ page }) => {
		await setAuth(page);
		await mockApi(page, { accounts: [] });

		await page.goto('/settings/connected-accounts');

		await expect(page.getByText('Available Integrations')).toBeVisible();
		await expect(page.getByText('Slack', { exact: true })).toBeVisible();
		await expect(page.getByText('Channels, messages, and search')).toBeVisible();
	});
});
