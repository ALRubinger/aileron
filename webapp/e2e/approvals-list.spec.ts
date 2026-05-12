import { test, expect, fixture } from './fixtures/approvals';

test.describe('Approvals page — listing', () => {
	test('shows the empty-state hint when the snapshot is empty', async ({
		page,
		approvalsMock
	}) => {
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([]);
		await expect(
			page.getByText(/No pending approvals/i)
		).toBeVisible();
	});

	test('renders a populated snapshot — one card per pending entry', async ({
		page,
		approvalsMock
	}) => {
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([
			fixture.action(),
			fixture.commsSend(),
			fixture.httpRequest()
		]);

		const cards = page.getByTestId('action-approval-card');
		await expect(cards).toHaveCount(3);
		await expect(
			page.locator('[data-approval-id="act-action-1"]')
		).toBeVisible();
		await expect(
			page.locator('[data-approval-id="act-comms-send-1"]')
		).toBeVisible();
		await expect(
			page.locator('[data-approval-id="act-http-1"]')
		).toBeVisible();
	});

	test('appends a card when a pending event arrives after load', async ({
		page,
		approvalsMock
	}) => {
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([fixture.action()]);
		await expect(page.getByTestId('action-approval-card')).toHaveCount(1);

		await approvalsMock.sse.pushPending(
			fixture.commsSend({ id: 'act-late-arrival' })
		);

		await expect(page.getByTestId('action-approval-card')).toHaveCount(2);
		await expect(
			page.locator('[data-approval-id="act-late-arrival"]')
		).toBeVisible();
	});

	test('removes a card when a resolved event arrives for that id', async ({
		page,
		approvalsMock
	}) => {
		// A `resolved` event the user did NOT trigger themselves —
		// simulates a second tab (or the CLI) deciding the approval.
		// The page must drop the matching card on receipt.
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([
			fixture.action(),
			fixture.commsSend()
		]);
		await expect(page.getByTestId('action-approval-card')).toHaveCount(2);

		await approvalsMock.sse.pushResolved(
			fixture.resolved('act-action-1', true)
		);

		await expect(page.getByTestId('action-approval-card')).toHaveCount(1);
		await expect(
			page.locator('[data-approval-id="act-action-1"]')
		).toHaveCount(0);
	});

	test('de-dupes a `pending` event for an id already in the snapshot', async ({
		page,
		approvalsMock
	}) => {
		// SSE reconnect can replay a snapshot whose ids overlap with
		// recently-delivered `pending` events. The page must not double
		// the cards.
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([fixture.action()]);
		await approvalsMock.sse.pushPending(fixture.action());

		await expect(page.getByTestId('action-approval-card')).toHaveCount(1);
	});
});
