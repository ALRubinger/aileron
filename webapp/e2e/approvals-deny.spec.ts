import { test, expect, fixture } from './fixtures/approvals';

test.describe('Approvals page — denying', () => {
	test('Deny with no reason posts {approved: false, reason: ""}', async ({
		page,
		approvalsMock
	}) => {
		// Per webapp/src/routes/approvals/+page.svelte:70 the reason
		// field is optional on deny — the page forwards "" verbatim
		// rather than blocking submit. The runtime forwards "" to the
		// agent which reads it as a bare "user denied".
		const item = fixture.action();
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([item]);

		await page
			.locator(`[data-approval-id="${item.id}"]`)
			.getByTestId('deny-button')
			.click();

		await expect.poll(() => approvalsMock.decides.posts.length).toBe(1);
		const post = approvalsMock.decides.posts[0];
		expect(post.approvalId).toBe(item.id);
		expect(post.body.approved).toBe(false);
		expect(post.body.reason).toBe('');

		await expect(
			page.locator(`[data-approval-id="${item.id}"]`)
		).toHaveCount(0);
	});

	test('Deny with a typed reason ships the reason on the POST', async ({
		page,
		approvalsMock
	}) => {
		const item = fixture.commsSend();
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([item]);

		const card = page.locator(`[data-approval-id="${item.id}"]`);
		await card.getByTestId('deny-reason-input').fill('wrong channel');
		await card.getByTestId('deny-button').click();

		await expect.poll(() => approvalsMock.decides.posts.length).toBe(1);
		const post = approvalsMock.decides.posts[0];
		expect(post.approvalId).toBe(item.id);
		expect(post.body.approved).toBe(false);
		expect(post.body.reason).toBe('wrong channel');
	});

	test('Approve ignores whatever is in the reason input', async ({
		page,
		approvalsMock
	}) => {
		// The input is shared between approve and deny; the page
		// snapshots reason='' on approve so a stray typed value doesn't
		// leak into an approval audit event.
		const item = fixture.action();
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([item]);

		const card = page.locator(`[data-approval-id="${item.id}"]`);
		await card.getByTestId('deny-reason-input').fill('this should not ship');
		await card.getByTestId('approve-button').click();

		await expect.poll(() => approvalsMock.decides.posts.length).toBe(1);
		expect(approvalsMock.decides.posts[0].body.approved).toBe(true);
		expect(approvalsMock.decides.posts[0].body.reason).toBe('');
	});

	test('denying one entry leaves the others in place', async ({
		page,
		approvalsMock
	}) => {
		const a = fixture.action();
		const b = fixture.httpRequest();
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([a, b]);

		await page
			.locator(`[data-approval-id="${b.id}"]`)
			.getByTestId('deny-button')
			.click();

		await expect.poll(() => approvalsMock.decides.posts.length).toBe(1);
		expect(approvalsMock.decides.posts[0].approvalId).toBe(b.id);
		await expect(
			page.locator(`[data-approval-id="${a.id}"]`)
		).toBeVisible();
		await expect(
			page.locator(`[data-approval-id="${b.id}"]`)
		).toHaveCount(0);
	});
});
