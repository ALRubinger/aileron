import { test, expect } from './coverage-fixture';

const mockAccounts = [
	{
		id: 'conn_1',
		provider: 'slack',
		external_user_id: 'U123SLACK',
		status: 'active',
		scopes: ['channels:read', 'chat:write'],
		created_at: '2025-01-15T00:00:00Z',
		updated_at: '2025-01-15T00:00:00Z'
	},
	{
		id: 'conn_2',
		provider: 'gmail',
		external_user_id: 'user@gmail.com',
		status: 'active',
		scopes: ['gmail.readonly'],
		created_at: '2025-02-20T00:00:00Z',
		updated_at: '2025-02-20T00:00:00Z'
	}
];

// Intercept API calls and mock auth so the page loads without a real backend
function mockApi(page: import('@playwright/test').Page, accounts = mockAccounts) {
	return Promise.all([
		// Mock connected accounts list
		page.route('**/v1/connected-accounts', (route) => {
			if (route.request().method() === 'GET') {
				return route.fulfill({ json: { items: accounts } });
			}
			if (route.request().method() === 'DELETE') {
				return route.fulfill({ status: 204 });
			}
			return route.continue();
		}),
		// Mock vault status — unlocked and ready
		page.route('**/v1/users/me/vault/status', (route) =>
			route.fulfill({
				json: { locked: false, has_passphrase: true }
			})
		),
		// Mock user endpoint (required by auth)
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

// Inject a fake auth token so the app thinks the user is logged in
async function setAuth(page: import('@playwright/test').Page) {
	await page.addInitScript(() => {
		localStorage.setItem('aileron_access_token', 'fake-token-for-e2e');
	});
}

test.describe('Connected Accounts Page', () => {
	test('renders connected accounts and available integrations', async ({ page }) => {
		await setAuth(page);
		await mockApi(page);

		await page.goto('/settings/connected-accounts');

		// Connected accounts section
		await expect(page.getByText('Slack', { exact: true })).toBeVisible();
		await expect(page.getByText('U123SLACK')).toBeVisible();
		await expect(page.getByText('Gmail', { exact: true })).toBeVisible();
		await expect(page.getByText('user@gmail.com')).toBeVisible();

		// Available integrations (not yet connected)
		await expect(page.getByText('GitHub')).toBeVisible();
		await expect(page.getByText('Google Calendar')).toBeVisible();
	});

	test('shows empty state when no accounts connected', async ({ page }) => {
		await setAuth(page);
		await mockApi(page, []);

		await page.goto('/settings/connected-accounts');

		await expect(page.getByText('No accounts connected yet.')).toBeVisible();
		// All 4 providers should be available
		await expect(page.getByText('Slack', { exact: true })).toBeVisible();
		await expect(page.getByText('GitHub')).toBeVisible();
		await expect(page.getByText('Gmail', { exact: true })).toBeVisible();
		await expect(page.getByText('Google Calendar')).toBeVisible();
	});

	test('Connect button navigates to OAuth URL', async ({ page }) => {
		await setAuth(page);
		await mockApi(page, []);

		await page.goto('/settings/connected-accounts');

		// Wait for page to load
		await expect(page.getByText('Slack', { exact: true })).toBeVisible();

		// Click Connect on Slack — it should navigate to the OAuth endpoint
		const connectButtons = page.getByRole('button', { name: 'Connect' });
		const [request] = await Promise.all([
			page.waitForRequest((req) => req.url().includes('/v1/connect/')),
			connectButtons.first().click()
		]);

		expect(request.url()).toContain('/v1/connect/');
	});

	test('disconnect flow shows confirmation dialog', async ({ page }) => {
		await setAuth(page);
		await mockApi(page);

		await page.goto('/settings/connected-accounts');

		await expect(page.getByText('Slack', { exact: true })).toBeVisible();

		// Click Disconnect on the first account
		const disconnectButtons = page.getByRole('button', { name: 'Disconnect' });
		await disconnectButtons.first().click();

		// Dialog should appear
		await expect(page.getByText(/Disconnect Slack\?/)).toBeVisible();
		await expect(page.getByText('This will remove the connection')).toBeVisible();

		// Cancel button closes the dialog
		await page.getByRole('button', { name: 'Cancel' }).click();
		await expect(page.getByText(/Disconnect Slack\?/)).not.toBeVisible();
	});

	test('settings sidebar has Connected Accounts link', async ({ page }) => {
		await setAuth(page);
		await mockApi(page);

		await page.goto('/settings/profile');

		// Navigate via sidebar
		const navLink = page.getByRole('link', { name: 'Connected Accounts' });
		await expect(navLink).toBeVisible();
		await navLink.click();

		await expect(page).toHaveURL(/\/settings\/connected-accounts/);
	});
});
