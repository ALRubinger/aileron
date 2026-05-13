import { test, expect, fixture } from './fixtures/approvals';

/**
 * `[approval.preview]` rendering on the /approvals page — ADR-0016.
 *
 * The preview payload is attached to the pending-approval entry by the
 * daemon's `RunAction` handler before the approval enters the queue.
 * The webapp renders the rendered {label, value} fields (or the
 * wholesale "preview unavailable: <reason>" fallback) alongside the
 * gated action's raw args so the user has an authoritative summary of
 * what they are approving.
 *
 * These specs exercise the full success / per-field-miss / wholesale-
 * failure surface against the production approval card. The wire shape
 * mirrors the OpenAPI definition (`ActionApprovalPreview`) so the page
 * does not need to ship with bespoke decoding.
 */
test.describe('Approvals page — [approval.preview] rendering (ADR-0016)', () => {
	test('renders rendered preview fields in manifest declaration order', async ({
		page,
		approvalsMock
	}) => {
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([fixture.actionWithPreview()]);

		// The preview block itself is visible alongside the raw args.
		const card = page.locator('[data-approval-id="act-preview-1"]');
		await expect(card.getByTestId('approval-preview')).toBeVisible();
		await expect(
			card.getByTestId('approval-preview-fields')
		).toBeVisible();

		// Order is load-bearing: the manifest declared To, Subject,
		// Preview in that sequence, and the user expects the prompt to
		// match. Without order preservation the prompt would shuffle on
		// every render and confuse the user about what they're approving.
		const fields = card.getByTestId('approval-preview-field');
		await expect(fields).toHaveCount(3);
		await expect(fields.nth(0)).toHaveAttribute('data-field-label', 'To');
		await expect(fields.nth(0)).toContainText('alice@example.com');
		await expect(fields.nth(1)).toHaveAttribute('data-field-label', 'Subject');
		await expect(fields.nth(1)).toContainText('Weekly recap');
		await expect(fields.nth(2)).toHaveAttribute('data-field-label', 'Preview');
		await expect(fields.nth(2)).toContainText('Here\'s the recap from this week\'s standup...');
	});

	test('renders "n/a" for fields whose render path missed', async ({
		page,
		approvalsMock
	}) => {
		// Per ADR-0016, when an individual render path does not resolve
		// in the preview op's response the field surfaces with
		// missing=true so the user sees that the upstream did not
		// return that key — rather than the row being silently absent
		// (which could be read as "no recipients" instead of "we don't
		// know").
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([
			fixture.actionWithPreview(undefined, {
				fields: [
					fixture.previewField('To', { value: 'alice@example.com' }),
					fixture.previewField('Subject', { missing: true }),
					fixture.previewField('Preview', { missing: true })
				]
			})
		]);

		const card = page.locator('[data-approval-id="act-preview-1"]');
		const subjectRow = card.locator('[data-field-label="Subject"]');
		await expect(subjectRow).toHaveAttribute('data-field-missing', 'true');
		await expect(subjectRow).toContainText('n/a');
		const previewRow = card.locator('[data-field-label="Preview"]');
		await expect(previewRow).toContainText('n/a');
		// Resolved field still renders normally.
		const toRow = card.locator('[data-field-label="To"]');
		await expect(toRow).toHaveAttribute('data-field-missing', 'false');
		await expect(toRow).toContainText('alice@example.com');
	});

	test('renders the wholesale-failure reason when the preview op failed', async ({
		page,
		approvalsMock
	}) => {
		// ADR-0016 failure-modes table: HTTP non-2xx, timeout, sandbox
		// denial, WASM trap all collapse to a single "preview
		// unavailable: <reason>" surface. The approval still proceeds,
		// but the user must see that the preview itself failed so they
		// can decline rather than silently approving a doomed call.
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([
			fixture.actionWithPreview(
				{},
				fixture.previewUnavailable('preview unavailable: Gmail API returned 404')
			)
		]);

		const card = page.locator('[data-approval-id="act-preview-1"]');
		await expect(card.getByTestId('approval-preview-unavailable')).toContainText(
			'preview unavailable: Gmail API returned 404'
		);
		// Fields are not rendered on wholesale failure.
		await expect(
			card.getByTestId('approval-preview-fields')
		).toHaveCount(0);
		// The approve / deny buttons remain enabled — the user can
		// still decide even without the preview.
		await expect(card.getByTestId('approve-button')).toBeEnabled();
		await expect(card.getByTestId('deny-button')).toBeEnabled();
	});

	test('omits the preview block when no [approval.preview] was declared', async ({
		page,
		approvalsMock
	}) => {
		// Manifests without an `[approval.preview]` block render the
		// existing args card. The preview slot must not appear at all
		// (no empty header, no "n/a" placeholder for the block itself).
		// The args accordion is still rendered so the user can opt into
		// seeing the raw agent inputs.
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([fixture.action()]);

		const card = page.locator('[data-approval-id="act-action-1"]');
		await expect(card.getByTestId('approval-preview')).toHaveCount(0);
		// The accordion's trigger is visible; the args body is hidden
		// until the user expands it (ADR-0016 IA: human-readable preview
		// is primary, raw inputs are opt-in detail).
		await expect(card.getByTestId('approval-args-trigger')).toBeVisible();
		await expect(card.getByTestId('approval-args')).toBeHidden();
	});

	test('action args render in a collapsed-by-default raw-JSON accordion', async ({
		page,
		approvalsMock
	}) => {
		// IA contract for the action kind: the raw args object is a
		// noisy fallback for power users, not the primary signal. It
		// must collapse so the human-readable preview (when present) or
		// the labeled input_fields block (when present) is what the
		// user sees first. Clicking the trigger reveals the raw JSON
		// for transparency. The trigger label is "Raw inputs (JSON)"
		// per the ADR-0003 amendment that introduced server-side
		// projection of args through `[[inputs]]`.
		await page.goto('/approvals');
		await approvalsMock.sse.pushSnapshot([fixture.action()]);

		const card = page.locator('[data-approval-id="act-action-1"]');
		const trigger = card.getByTestId('approval-args-trigger');
		const body = card.getByTestId('approval-args');

		await expect(trigger).toContainText('Raw inputs (JSON)');
		await expect(body).toBeHidden();

		await trigger.click();
		await expect(body).toBeVisible();
		// The args themselves still serialize verbatim so the
		// transparency case is preserved.
		await expect(body).toContainText('alice@example.com');
	});

	test('multiline preview fields render as scrollable blockquotes', async ({
		page,
		approvalsMock
	}) => {
		// ADR-0016 multiline directive: fields the manifest flagged as
		// `multiline` render as a blockquote below the inline rows so
		// long-form content (an email body, a commit message) is
		// readable instead of being truncated to a single line. Non-
		// multiline fields keep their inline `key: value` shape.
		await page.goto('/approvals');
		const longBody =
			"Hi Alice,\n\nHere's the recap from this week's standup. Lots of things happened. " +
			'We shipped X, we shipped Y, we deferred Z. Full details below.\n\n— Andrew';
		await approvalsMock.sse.pushSnapshot([
			fixture.actionWithPreview(undefined, {
				fields: [
					{ label: 'To', value: 'alice@example.com' },
					{ label: 'Subject', value: 'Weekly recap' },
					{ label: 'Body', value: longBody, multiline: true }
				]
			})
		]);

		const card = page.locator('[data-approval-id="act-preview-1"]');
		// Inline fields (To, Subject) stay in the dl.
		const inlineRows = card.getByTestId('approval-preview-field');
		await expect(inlineRows).toHaveCount(2);
		await expect(inlineRows.nth(0)).toHaveAttribute('data-field-label', 'To');
		await expect(inlineRows.nth(1)).toHaveAttribute('data-field-label', 'Subject');

		// The multiline Body field renders in its own block, not as a
		// dl row, and is in a blockquote element so the user gets a
		// visually distinct reading area for the long-form content.
		const block = card.getByTestId('approval-preview-multiline');
		await expect(block).toHaveCount(1);
		await expect(block).toHaveAttribute('data-field-label', 'Body');
		const quote = block.locator('blockquote');
		await expect(quote).toBeVisible();
		await expect(quote).toContainText('Hi Alice');
		await expect(quote).toContainText('— Andrew');
	});
});
