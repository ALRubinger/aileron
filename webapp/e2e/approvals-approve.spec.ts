import { test, expect, fixture } from './fixtures/approvals';

test.describe('Approvals page — approving', () => {
	test('action kind: clicking Approve posts {approved: true} for the right id', async ({
		page,
		approvalsMock
	}) => {
		const item = fixture.action();
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([item]);

		await expect(
			page.locator(`[data-approval-id="${item.id}"]`)
		).toBeVisible();

		const card = page.locator(`[data-approval-id="${item.id}"]`);
		await card.getByTestId('approve-button').click();

		// One decide POST landed against the right id with approved=true.
		// reason is empty on approve per +page.svelte:70.
		await expect.poll(() => approvalsMock.decides.posts.length).toBe(1);
		const post = approvalsMock.decides.posts[0];
		expect(post.approvalId).toBe(item.id);
		expect(post.body.approved).toBe(true);
		expect(post.body.reason).toBe('');
		expect(post.body.edited_payload).toBeUndefined();

		// Optimistic removal: the card disappears immediately on click
		// (page applies resolved synchronously). No SSE push needed.
		await expect(
			page.locator(`[data-approval-id="${item.id}"]`)
		).toHaveCount(0);
	});

	test('comms_send kind: card shows service/channel/body, Approve posts approved=true', async ({
		page,
		approvalsMock
	}) => {
		const item = fixture.commsSend();
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([item]);

		const card = page.locator(`[data-approval-id="${item.id}"]`);
		const summary = card.getByTestId('comms-send-summary');
		await expect(summary).toContainText('slack');
		await expect(summary).toContainText('#general');
		await expect(card.getByTestId('comms-send-body')).toContainText(
			'Deploying main to prod'
		);

		await card.getByTestId('approve-button').click();

		await expect.poll(() => approvalsMock.decides.posts.length).toBe(1);
		expect(approvalsMock.decides.posts[0].approvalId).toBe(item.id);
		expect(approvalsMock.decides.posts[0].body.approved).toBe(true);
	});

	test('http_request kind: card shows method + URL + credential, Approve posts approved=true', async ({
		page,
		approvalsMock
	}) => {
		const item = fixture.httpRequest();
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([item]);

		const card = page.locator(`[data-approval-id="${item.id}"]`);
		const summary = card.getByTestId('http-request-summary');
		await expect(summary).toContainText('POST');
		await expect(summary).toContainText('https://api.example.com/widgets');
		// secret_name should appear (without ever exposing the value).
		await expect(summary).toContainText('example_api_key');

		await card.getByTestId('approve-button').click();

		await expect.poll(() => approvalsMock.decides.posts.length).toBe(1);
		expect(approvalsMock.decides.posts[0].approvalId).toBe(item.id);
		expect(approvalsMock.decides.posts[0].body.approved).toBe(true);
	});

	test('http_request without a matching secret surfaces the unauthenticated hint', async ({
		page,
		approvalsMock
	}) => {
		const item = fixture.httpRequest({
			id: 'act-http-no-cred',
			args: {
				method: 'GET',
				url: 'https://api.example.com/public',
				body: '',
				secret_name: ''
			}
		});
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([item]);

		await expect(
			page.locator(`[data-approval-id="${item.id}"]`).getByText(/No matching api_key binding/i)
		).toBeVisible();
	});

	test('approving one of several entries leaves the others in place', async ({
		page,
		approvalsMock
	}) => {
		const a = fixture.action();
		const b = fixture.commsSend();
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([a, b]);

		await page
			.locator(`[data-approval-id="${a.id}"]`)
			.getByTestId('approve-button')
			.click();

		await expect.poll(() => approvalsMock.decides.posts.length).toBe(1);
		await expect(
			page.locator(`[data-approval-id="${a.id}"]`)
		).toHaveCount(0);
		await expect(
			page.locator(`[data-approval-id="${b.id}"]`)
		).toBeVisible();
	});
});
