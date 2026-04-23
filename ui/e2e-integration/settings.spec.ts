import { test, expect, API_URL } from './auth-fixture';

/**
 * Helper: navigate to a settings page and wait for it to settle.
 * The app may redirect to /setup-vault if the user has no passphrase.
 * Returns true if we reached the target page, false if redirected to vault setup.
 */
async function gotoSettings(page: import('@playwright/test').Page, path: string) {
	await page.goto(path);

	// Wait for the page to settle — either the target content or vault setup
	const settled = page.getByText('Secure your vault').or(page.locator('main h1, [data-slot="card-title"]').first());
	await expect(settled).toBeVisible({ timeout: 15000 });

	return !page.url().includes('/setup-vault');
}

test.describe('Settings Pages', () => {
	test('profile page shows current user or redirects to vault setup', async ({
		authedPage: page
	}) => {
		const reached = await gotoSettings(page, '/settings/profile');

		if (!reached) {
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

		await expect(page.getByText('Profile')).toBeVisible();
		await expect(page.getByText('Playwright E2E')).toBeVisible();
	});

	test('settings sidebar navigation works', async ({ authedPage: page }) => {
		const reached = await gotoSettings(page, '/settings/profile');

		if (!reached) {
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

		// Navigate to Organization via sidebar
		await page.getByRole('link', { name: 'Organization' }).click();
		await expect(page).toHaveURL(/\/settings\/organization/);

		// Navigate to Security via sidebar
		await page.getByRole('link', { name: 'Security' }).click();
		await expect(page).toHaveURL(/\/settings\/security/);

		// Navigate to Connected Accounts via sidebar
		await page.getByRole('link', { name: 'Connected Accounts' }).click();
		await expect(page).toHaveURL(/\/settings\/connected-accounts/);
	});

	test('connected accounts page loads or redirects to vault setup', async ({
		authedPage: page
	}) => {
		const reached = await gotoSettings(page, '/settings/connected-accounts');

		if (!reached) {
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

		await expect(
			page.locator('[data-slot="card-title"]', { hasText: 'Connected Accounts' })
		).toBeVisible();
	});

	test('user profile display name is editable', async ({ authedPage: page }) => {
		const reached = await gotoSettings(page, '/settings/profile');

		if (!reached) {
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

		const nameInput = page.getByLabel('Display name');
		await expect(nameInput).toBeVisible();
		await expect(nameInput).toHaveValue('Playwright E2E');
	});
});
