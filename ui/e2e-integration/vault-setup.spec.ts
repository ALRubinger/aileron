import { test, expect, ensureAuth, API_URL } from './auth-fixture';

test.describe('Vault Setup Flow', () => {
	test('new user is redirected to vault setup', async ({ authedPage: page }) => {
		// A fresh test user with no passphrase should be redirected to setup.
		await page.goto('/settings/connected-accounts');

		// The app should redirect to /vault since no passphrase exists.
		await expect(page).toHaveURL(/\/vault/, { timeout: 10000 });
		await expect(page.getByText('Secure your vault')).toBeVisible();
	});

	test('vault setup page has passphrase form', async ({ authedPage: page }) => {
		await page.goto('/vault');

		await expect(page.getByLabel('Vault passphrase')).toBeVisible();
		await expect(page.getByLabel('Confirm passphrase')).toBeVisible();
		await expect(page.getByRole('button', { name: 'Continue' })).toBeVisible();
	});

	test('vault setup rejects short passphrase', async ({ authedPage: page }) => {
		await page.goto('/vault');

		await page.getByLabel('Vault passphrase').fill('short');
		await page.getByLabel('Confirm passphrase').fill('short');
		await page.getByRole('button', { name: 'Continue' }).click();

		await expect(page.getByText('at least 8 characters')).toBeVisible();
	});

	test('vault setup rejects mismatched passphrases', async ({ authedPage: page }) => {
		await page.goto('/vault');

		await page.getByLabel('Vault passphrase').fill('secure-passphrase-1');
		await page.getByLabel('Confirm passphrase').fill('secure-passphrase-2');
		await page.getByRole('button', { name: 'Continue' }).click();

		await expect(page.getByText('do not match')).toBeVisible();
	});
});
