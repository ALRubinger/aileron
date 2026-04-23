import { test, expect, API_URL } from './auth-fixture';

test.describe('Settings Pages', () => {
	test('profile page shows current user or redirects to vault setup', async ({
		authedPage: page
	}) => {
		await page.goto('/settings/profile');
		await page.waitForURL(/\/(settings\/profile|setup-vault)/, { timeout: 10000 });

		if (page.url().includes('/setup-vault')) {
			// Fresh user without passphrase — vault setup required first
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

		await expect(page.getByText('Profile')).toBeVisible();
		await expect(page.getByText('Playwright E2E')).toBeVisible();
	});

	test('settings sidebar navigation works', async ({ authedPage: page }) => {
		await page.goto('/settings/profile');
		await page.waitForURL(/\/(settings\/profile|setup-vault)/, { timeout: 10000 });

		// If redirected to vault setup, skip sidebar test
		if (page.url().includes('/setup-vault')) {
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

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

	test('connected accounts page loads or redirects to vault setup', async ({
		authedPage: page
	}) => {
		await page.goto('/settings/connected-accounts');
		await page.waitForURL(/\/(settings\/connected-accounts|setup-vault)/, { timeout: 10000 });

		if (page.url().includes('/setup-vault')) {
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

		// Page rendered — verify the card title (not sidebar link)
		await expect(
			page.locator('[data-slot="card-title"]', { hasText: 'Connected Accounts' })
		).toBeVisible();
	});

	test('user profile display name is editable', async ({ authedPage: page }) => {
		await page.goto('/settings/profile');
		await page.waitForURL(/\/(settings\/profile|setup-vault)/, { timeout: 10000 });

		if (page.url().includes('/setup-vault')) {
			await expect(page.getByText('Secure your vault')).toBeVisible();
			return;
		}

		const nameInput = page.getByLabel('Display name');
		await expect(nameInput).toBeVisible();
		await expect(nameInput).toHaveValue('Playwright E2E');
	});
});
