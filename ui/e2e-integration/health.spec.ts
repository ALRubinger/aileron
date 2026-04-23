import { test, expect } from '@playwright/test';

const API_URL = process.env.AILERON_API_URL || 'http://localhost:8080';

test.describe('Health Check', () => {
	test('API server is healthy', async ({ request }) => {
		const resp = await request.get(`${API_URL}/v1/health`);
		expect(resp.ok()).toBe(true);
	});

	test('UI serves the login page', async ({ page }) => {
		await page.goto('/login');
		await expect(page).toHaveTitle(/Aileron/);
	});
});
