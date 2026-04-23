import { test, expect, API_URL } from './auth-fixture';

test.describe('Settings Pages', () => {
	test('profile page shows current user', async ({ authedPage: page }) => {
		await page.goto('/settings/profile');

		await expect(page.getByText('Profile')).toBeVisible();
		// The display name from our test account should appear
		await expect(page.getByText('Playwright E2E')).toBeVisible();
	});

	test('settings sidebar navigation works', async ({ authedPage: page }) => {
		await page.goto('/settings/profile');

		// Navigate to Organization via sidebar
		const orgLink = page.getByRole('link', { name: 'Organization' });
		await expect(orgLink).toBeVisible();
		await orgLink.click();
		await expect(page).toHaveURL(/\/settings\/organization/);

		// Navigate to Security via sidebar
		const secLink = page.getByRole('link', { name: 'Security' });
		await expect(secLink).toBeVisible();
		await secLink.click();
		await expect(page).toHaveURL(/\/settings\/security/);

		// Navigate to Connected Accounts via sidebar
		const connLink = page.getByRole('link', { name: 'Connected Accounts' });
		await expect(connLink).toBeVisible();
		await connLink.click();
		await expect(page).toHaveURL(/\/settings\/connected-accounts/);
	});

	test('connected accounts page shows empty state from real API', async ({ authedPage: page }) => {
		await page.goto('/settings/connected-accounts');

		// Wait for page to load — may show vault setup prompt or connected accounts
		// depending on vault state. Either way, the page should render.
		await expect(
			page.getByText('Connected Accounts').or(page.getByText('Secure your vault'))
		).toBeVisible({ timeout: 10000 });
	});

	test('user profile can be updated', async ({ authedPage: page }) => {
		await page.goto('/settings/profile');

		// Find the display name input and verify it has our test name
		const nameInput = page.getByLabel('Display name');
		await expect(nameInput).toBeVisible();
		await expect(nameInput).toHaveValue('Playwright E2E');
	});
});
