import { test, expect, type Page } from '@playwright/test';

// Real-daemon integration test for the /approvals page.
//
// Setup expectation: an `aileron` daemon is already running with the
// `test/fixtures/action-approvals/basic.json` seed loaded (three
// pending entries: `action`, `comms_send`, `http_request`) and its
// bound URL is in `AILERON_WEBAPP_URL`. The Taskfile target
// `test:webapp:e2e:integration` is responsible for that wiring.
//
// The tests share a single daemon and the daemon's queue is mutable
// state. They run sequentially (workers=1 in playwright.integration.config.ts)
// and each test draws down a different entry from the seed so they
// don't trip over each other. Re-running the suite without restarting
// the daemon would fail the second run — that's a feature, not a bug:
// the seed represents a known starting state.

test.beforeAll(async () => {
	if (!process.env.AILERON_WEBAPP_URL) {
		throw new Error(
			'AILERON_WEBAPP_URL is not set. Run via `task test:webapp:e2e:integration` so the orchestration script starts the daemon and exports the URL.'
		);
	}
});

test('lists the seeded pending approvals', async ({ page }) => {
	await page.goto('/approvals');

	// Three cards from the seed fixture, no mocks.
	await expect(page.getByTestId('action-approval-card')).toHaveCount(3, {
		timeout: 10_000
	});
	const kinds = await page
		.getByTestId('action-approval-card')
		.evaluateAll((els) => els.map((e) => (e as HTMLElement).dataset.approvalKind));
	expect(kinds).toEqual(expect.arrayContaining(['action', 'comms_send', 'http_request']));
});

test('approving the action kind removes its card and clears it from the server queue', async ({
	page
}) => {
	await page.goto('/approvals');
	const card = firstCardByKind(page, 'action');
	await expect(card).toBeVisible();
	const approvalId = await card.getAttribute('data-approval-id');
	if (!approvalId) throw new Error('action card missing data-approval-id');

	await card.getByTestId('approve-button').click();

	// Optimistic removal on the client.
	await expect(card).toHaveCount(0);

	// Server-side state agrees: the entry no longer appears in the
	// pending list.
	const remaining = await fetchPendingIds(page);
	expect(remaining).not.toContain(approvalId);
});

test('denying the comms_send kind with a reason persists the reason on the server', async ({
	page
}) => {
	await page.goto('/approvals');
	const card = firstCardByKind(page, 'comms_send');
	await expect(card).toBeVisible();
	const approvalId = await card.getAttribute('data-approval-id');
	if (!approvalId) throw new Error('comms_send card missing data-approval-id');

	await card.getByTestId('deny-reason-input').fill('wrong channel');
	await card.getByTestId('deny-button').click();

	await expect(card).toHaveCount(0);

	// The decided entry is retained in the queue's outcome map; poll
	// the result endpoint to confirm the deny reason round-tripped.
	const result = await fetchResult(page, approvalId);
	expect(result.status).toBe('denied');
	expect(result.reason).toBe('wrong channel');
});

test('approving the http_request kind drains the remaining card and shows the empty state', async ({
	page
}) => {
	await page.goto('/approvals');
	const card = firstCardByKind(page, 'http_request');
	await expect(card).toBeVisible();

	await card.getByTestId('approve-button').click();
	await expect(card).toHaveCount(0);

	// Empty state renders once the queue drains.
	await expect(page.getByText(/No pending approvals/i)).toBeVisible();
});

function firstCardByKind(page: Page, kind: string) {
	return page.locator(`[data-approval-kind="${kind}"]`).first();
}

async function fetchPendingIds(page: Page): Promise<string[]> {
	await ensureDaemonAuth(page);
	const resp = await page.request.get('/v1/action-approvals');
	expect(resp.ok()).toBe(true);
	const body = (await resp.json()) as {
		items: Array<{ id: string }>;
	};
	return body.items.map((i) => i.id);
}

async function fetchResult(
	page: Page,
	id: string
): Promise<{ status: string; reason?: string }> {
	await ensureDaemonAuth(page);
	const resp = await page.request.get(`/v1/action-approvals/${id}/result`);
	expect(resp.ok()).toBe(true);
	return resp.json();
}

async function ensureDaemonAuth(page: Page): Promise<void> {
	const resp = await page.request.get('/v1/auth/handshake');
	expect([200, 204]).toContain(resp.status());
}
