import { describe, it, expect, vi, beforeAll, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	watchActionApprovals: vi.fn(),
	decideActionApproval: vi.fn()
}));

import Page from './+page.svelte';
import {
	watchActionApprovals,
	decideActionApproval,
	type ActionApprovalSubscriber,
	type PendingActionApproval
} from '$lib/api';

// captured holds the most recent subscriber the page handed to
// watchActionApprovals. Tests fire `pending` / `resolved` events by
// invoking these callbacks directly — same shape the SSE handler
// would, without needing to stand up an EventSource.
let captured: ActionApprovalSubscriber | null = null;

function setupWatcher(initial: PendingActionApproval[] = []) {
	vi.mocked(watchActionApprovals).mockImplementation((sub) => {
		captured = sub;
		// Snapshot delivery has to be deferred so the page's onMount
		// has fully run by the time the state update fires; otherwise
		// the reactive update lands before the component is mounted
		// and Svelte logs a warning.
		queueMicrotask(() => sub.onSnapshot(initial));
		return () => {
			captured = null;
		};
	});
}

beforeEach(() => {
	captured = null;
	vi.mocked(decideActionApproval).mockResolvedValue(null);
	setupWatcher();
	// Clear any ?focus= left over from a prior test — the page reads
	// window.location.search on mount, so a leak would silently break
	// unrelated cases.
	window.history.replaceState({}, '', '/approvals');
});

describe('Approvals page — empty state', () => {
	it('shows the empty-state hint when no approvals are pending', async () => {
		render(Page);
		await waitFor(() => {
			expect(
				screen.getByText(/No pending approvals\. The agent's blocked tool calls/)
			).toBeInTheDocument();
		});
	});
});

describe('Approvals page — action approvals (#418)', () => {
	it('renders pending action approvals from the SSE snapshot', async () => {
		setupWatcher([
			{
				id: 'act-test-1',
				kind: 'action' as const,
			action_name: 'send-email',
				connector_fqn: 'github://x/aileron-connector-google',
				args: { to: 'alice@example.com', subject: 'hi' },
				session_id: 'session-42',
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);

		render(Page);

		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});
		expect(screen.getByText(/github:\/\/x\/aileron-connector-google/)).toBeInTheDocument();
		const argsBlock = screen.getByTestId('approval-args');
		expect(argsBlock.textContent).toContain('alice@example.com');
		expect(argsBlock.textContent).toContain('subject');
		expect(screen.getByText(/Session: session-42/)).toBeInTheDocument();
	});

	it('renders the (no args) placeholder when an entry has no args', async () => {
		setupWatcher([
			{
				id: 'act-test-1',
				kind: 'action' as const,
			action_name: 'noop',
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('noop')).toBeInTheDocument();
		});
		expect(screen.getByTestId('approval-args').textContent).toBe('(no args)');
	});

	it('appends a card when a `pending` SSE event arrives', async () => {
		render(Page);
		await waitFor(() => {
			expect(captured).not.toBeNull();
		});

		captured!.onPending({
			id: 'act-test-2',
			kind: 'action' as const,
			action_name: 'send-email',
			requested_at: '2026-05-04T12:00:01Z'
		});

		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});
	});

	it('removes a card when a `resolved` SSE event arrives', async () => {
		setupWatcher([
			{
				id: 'act-test-3',
				kind: 'action' as const,
			action_name: 'send-email',
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});

		captured!.onResolved({
			id: 'act-test-3',
			approved: true,
			decided_at: '2026-05-04T12:00:05Z'
		});

		await waitFor(() => {
			expect(screen.queryByText('send-email')).not.toBeInTheDocument();
		});
	});

	it('de-dupes when a snapshot replay covers the same id', async () => {
		setupWatcher([
			{
				id: 'act-test-4',
				kind: 'action' as const,
			action_name: 'send-email',
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});

		// Simulate a `pending` event for the same id (could happen on a
		// reconnect race between snapshot and Subscribe). The card
		// must not duplicate.
		captured!.onPending({
			id: 'act-test-4',
			kind: 'action' as const,
			action_name: 'send-email',
			requested_at: '2026-05-04T12:00:00Z'
		});
		const cards = await screen.findAllByTestId('action-approval-card');
		expect(cards).toHaveLength(1);
	});

	it('approves an action and removes it from the list optimistically', async () => {
		setupWatcher([
			{
				id: 'act-test-1',
				kind: 'action' as const,
			action_name: 'send-email',
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);

		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});

		const approveButton = screen.getByTestId('approve-button');
		await fireEvent.click(approveButton);

		await waitFor(() => {
			expect(decideActionApproval).toHaveBeenCalledWith('act-test-1', true, '', undefined);
		});
		await waitFor(() => {
			expect(screen.queryByText('send-email')).not.toBeInTheDocument();
		});
	});

	it('denies an action with the typed reason', async () => {
		setupWatcher([
			{
				id: 'act-test-1',
				kind: 'action' as const,
			action_name: 'send-email',
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);

		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});

		const reasonInput = screen.getByTestId('deny-reason-input') as HTMLInputElement;
		await fireEvent.input(reasonInput, { target: { value: 'wrong recipient' } });
		const denyButton = screen.getByTestId('deny-button');
		await fireEvent.click(denyButton);

		await waitFor(() => {
			expect(decideActionApproval).toHaveBeenCalledWith(
				'act-test-1',
				false,
				'wrong recipient',
				undefined
			);
		});
	});

	it('shows the SSE error message instead of the "Connecting…" placeholder when the stream fails', async () => {
		// Regression: when the SSE handler fails (e.g. 500
		// streaming_unsupported from a missing Flush in middleware),
		// the page must surface the error rather than sit on the
		// "Connecting to the approval stream…" placeholder forever.
		vi.mocked(watchActionApprovals).mockImplementation((sub) => {
			queueMicrotask(() => sub.onError?.(new Error('stream closed: 500')));
			return () => {};
		});
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('stream closed: 500')).toBeInTheDocument();
		});
		expect(screen.queryByText(/Connecting to the approval stream/)).not.toBeInTheDocument();
	});

	it('surfaces server errors from decideActionApproval', async () => {
		setupWatcher([
			{
				id: 'act-test-1',
				kind: 'action' as const,
			action_name: 'send-email',
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);
		vi.mocked(decideActionApproval).mockRejectedValue(
			new Error('approval is unknown or already resolved')
		);

		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});

		await fireEvent.click(screen.getByTestId('approve-button'));

		await waitFor(() => {
			expect(screen.getByText('approval is unknown or already resolved')).toBeInTheDocument();
		});
	});
});

// Input-fields rendering on action-kind approvals: the server
// projects the gated action's call-time args through its manifest's
// [[inputs]] declarations (per the ADR-0003 amendment) so the user
// sees labeled rows instead of a raw JSON dump. These tests pin the
// contract observable from the wire shape outward — labels render in
// declared order, multiline fields render as scrollable blockquotes
// with real newlines, and the JSON accordion stays collapsed as the
// expert-mode fallback.
describe('Approvals page — input_fields rendering (ADR-0003 amendment)', () => {
	it('renders labeled rows for declared inputs in order', async () => {
		setupWatcher([
			{
				id: 'act-input-1',
				kind: 'action' as const,
				action_name: 'send-email',
				connector_fqn: 'github://x/aileron-connector-google',
				args: { to: 'alr@example.com', subject: 'Daily summary', body: 'Hello' },
				requested_at: '2026-05-04T12:00:00Z',
				input_fields: [
					{ label: 'To', value: 'alr@example.com' },
					{ label: 'Subject', value: 'Daily summary' },
					{ label: 'Body', value: 'Hello', multiline: true }
				]
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});
		// Inline rows surface via the approval-input-field test id; the
		// declaration order should be preserved as the DOM order.
		const inlineRows = screen.getAllByTestId('approval-input-field');
		expect(inlineRows.map((el) => el.getAttribute('data-field-label'))).toEqual([
			'To',
			'Subject'
		]);
		expect(inlineRows[0].textContent).toContain('alr@example.com');
		expect(inlineRows[1].textContent).toContain('Daily summary');
		// Multiline body renders as its own block, not as an inline row.
		const multilineBlock = screen.getByTestId('approval-input-multiline');
		expect(multilineBlock.getAttribute('data-field-label')).toBe('Body');
		expect(multilineBlock.textContent).toContain('Hello');
	});

	it('renders embedded newlines as real line breaks in multiline fields', async () => {
		// The user's complaint: today a body with \n appears as the
		// literal escape sequence inside the JSON dump. With the new
		// rendering path, the value carries actual newlines and the
		// blockquote's whitespace-pre-wrap CSS draws them as line
		// breaks. The wire value is a real-newline string; this test
		// asserts the renderer preserves it.
		const body = 'Line one\nLine two\n\nLine four';
		setupWatcher([
			{
				id: 'act-input-2',
				kind: 'action' as const,
				action_name: 'send-email',
				args: { body },
				requested_at: '2026-05-04T12:00:00Z',
				input_fields: [{ label: 'Body', value: body, multiline: true }]
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});
		const multilineBlock = screen.getByTestId('approval-input-multiline');
		const blockquote = multilineBlock.querySelector('blockquote');
		expect(blockquote).not.toBeNull();
		// The literal-newline contract: the rendered textContent
		// preserves the \n characters verbatim (the whitespace-pre-wrap
		// CSS class is what paints them as visible line breaks at
		// rendering time; jsdom doesn't compute styles, so we assert on
		// the content + the class presence).
		expect(blockquote!.textContent).toBe(body);
		expect(blockquote!.className).toContain('whitespace-pre-wrap');
		// The "\n" escape sequence should never appear literally in the
		// rendered DOM — that's the user-visible regression we are
		// guarding against.
		expect(blockquote!.textContent).not.toContain('\\n');
	});

	it('renders the multiline body as a user-resizable preview region', async () => {
		// The user must be able to read the *entire* body before
		// authorizing an irreversible send. The blockquote therefore
		// opens at a small preview height but exposes a vertical resize
		// handle (`resize-y`) so it can be dragged open to reveal the
		// whole value, with `overflow-y-auto` scrolling the remainder
		// until then. A `max-h` cap would stop the drag short of a long
		// body, so its absence is part of the contract this pins.
		const body = Array.from({ length: 40 }, (_, i) => `Line ${i + 1}`).join('\n');
		setupWatcher([
			{
				id: 'act-input-resize',
				kind: 'action' as const,
				action_name: 'send-email',
				args: { body },
				requested_at: '2026-05-04T12:00:00Z',
				input_fields: [{ label: 'Body', value: body, multiline: true }]
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});
		const blockquote = screen
			.getByTestId('approval-input-multiline')
			.querySelector('blockquote');
		expect(blockquote).not.toBeNull();
		// User-draggable vertical resize handle.
		expect(blockquote!.className).toContain('resize-y');
		// Small default preview height with a floor, scrollbar for the
		// overflow, and no upper cap that would clip a long body.
		expect(blockquote!.className).toContain('h-32');
		expect(blockquote!.className).toContain('min-h-16');
		expect(blockquote!.className).toContain('overflow-y-auto');
		expect(blockquote!.className).not.toContain('max-h-');
	});

	it('renders missing required inputs as "n/a"', async () => {
		setupWatcher([
			{
				id: 'act-input-3',
				kind: 'action' as const,
				action_name: 'send-email',
				args: { to: 'alr@example.com' },
				requested_at: '2026-05-04T12:00:00Z',
				input_fields: [
					{ label: 'To', value: 'alr@example.com' },
					{ label: 'Subject', missing: true }
				]
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});
		const subjectRow = screen
			.getAllByTestId('approval-input-field')
			.find((el) => el.getAttribute('data-field-label') === 'Subject');
		expect(subjectRow).toBeDefined();
		expect(subjectRow!.getAttribute('data-field-missing')).toBe('true');
		expect(subjectRow!.textContent).toContain('n/a');
	});

	it('keeps the JSON accordion collapsed and reachable as a fallback', async () => {
		setupWatcher([
			{
				id: 'act-input-4',
				kind: 'action' as const,
				action_name: 'send-email',
				args: { to: 'alr@example.com', subject: 'Hi' },
				requested_at: '2026-05-04T12:00:00Z',
				input_fields: [
					{ label: 'To', value: 'alr@example.com' },
					{ label: 'Subject', value: 'Hi' }
				]
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});
		// Labeled-field rendering is the authoritative summary above
		// the accordion.
		expect(screen.getByTestId('approval-input-fields-block')).toBeInTheDocument();
		// The accordion trigger still exists for debugging; its
		// content is collapsed by default (Radix renders the pre tag
		// only after the trigger is clicked, so its absence from the
		// initial DOM is the correct collapsed signal).
		expect(screen.getByTestId('approval-args-trigger')).toBeInTheDocument();
	});

	it('falls back to the raw-JSON accordion when input_fields is absent', async () => {
		setupWatcher([
			{
				id: 'act-input-5',
				kind: 'action' as const,
				action_name: 'send-email',
				args: { to: 'alr@example.com' },
				requested_at: '2026-05-04T12:00:00Z'
				// no input_fields — older daemon / manifest with no inputs
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});
		// The labeled block should not render.
		expect(screen.queryByTestId('approval-input-fields-block')).toBeNull();
		// The historic accordion still surfaces the JSON args.
		expect(screen.getByTestId('approval-args-trigger')).toBeInTheDocument();
	});
});

describe('Approvals page — comms-MCP kinds (#428)', () => {
	it('renders a comms_send card with service / channel / body', async () => {
		setupWatcher([
			{
				id: 'cs-1',
				kind: 'comms_send',
				action_name: 'send_message',
				args: { service: 'slack', channel: '#general', body: 'hi team' },
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('comms-send-summary')).toBeInTheDocument();
		});
		expect(screen.getByTestId('comms-send-body').textContent).toBe('hi team');
		expect(screen.getByText(/slack/)).toBeInTheDocument();
		expect(screen.getByText(/#general/)).toBeInTheDocument();
	});

	it('approves a comms_draft as-is when the user does not edit', async () => {
		setupWatcher([
			{
				id: 'cd-1',
				kind: 'comms_draft',
				action_name: 'draft_reply',
				args: {
					service: 'slack',
					channel: '#general',
					original_author: 'alice',
					original_body: 'ping',
					draft_body: 'pong',
					reply_to: 'msg-1'
				},
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('comms-draft-summary')).toBeInTheDocument();
		});
		const textarea = screen.getByTestId('comms-draft-body-input') as HTMLTextAreaElement;
		expect(textarea.value).toBe('pong');

		await fireEvent.click(screen.getByTestId('approve-button'));

		await waitFor(() => {
			expect(decideActionApproval).toHaveBeenCalledWith('cd-1', true, '', undefined);
		});
	});

	it('approves a comms_draft with the edited body in edited_payload', async () => {
		setupWatcher([
			{
				id: 'cd-2',
				kind: 'comms_draft',
				action_name: 'draft_reply',
				args: {
					service: 'slack',
					channel: '#general',
					original_author: 'alice',
					original_body: 'ping',
					draft_body: 'pong',
					reply_to: 'msg-2'
				},
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('comms-draft-body-input')).toBeInTheDocument();
		});

		const textarea = screen.getByTestId('comms-draft-body-input') as HTMLTextAreaElement;
		await fireEvent.input(textarea, { target: { value: 'pong — got it' } });
		await fireEvent.click(screen.getByTestId('approve-button'));

		await waitFor(() => {
			expect(decideActionApproval).toHaveBeenCalledWith('cd-2', true, '', {
				body: 'pong — got it'
			});
		});
	});

	it('renders an http_request card with the matched secret name', async () => {
		setupWatcher([
			{
				id: 'hr-1',
				kind: 'http_request',
				action_name: 'http_request',
				args: {
					method: 'POST',
					url: 'https://api.linear.app/graphql',
					body: '{"q":"x"}',
					secret_name: 'bindings/api_key/linear/work'
				},
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('http-request-summary')).toBeInTheDocument();
		});
		expect(screen.getByText('POST')).toBeInTheDocument();
		expect(screen.getByText(/api\.linear\.app/)).toBeInTheDocument();
		expect(screen.getByText(/bindings\/api_key\/linear\/work/)).toBeInTheDocument();
		expect(screen.getByTestId('http-request-body').textContent).toBe('{"q":"x"}');
	});

	it('http_request without a matched secret says the request goes out unauthenticated', async () => {
		setupWatcher([
			{
				id: 'hr-2',
				kind: 'http_request',
				action_name: 'http_request',
				args: {
					method: 'GET',
					url: 'https://example.com/healthz',
					body: '',
					secret_name: ''
				},
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);
		render(Page);
		await waitFor(() => {
			expect(
				screen.getByText(/No matching api_key binding/)
			).toBeInTheDocument();
		});
	});
});

describe('Approvals page — ?focus deep-link highlight', () => {
	beforeAll(() => {
		// jsdom doesn't implement scrollIntoView; the production code
		// calls it whenever a focused card lands in the DOM, so install a
		// stub at the prototype level so neither the spy nor the effect
		// itself blow up.
		if (!('scrollIntoView' in Element.prototype)) {
			(Element.prototype as unknown as { scrollIntoView: () => void }).scrollIntoView =
				() => {};
		}
	});

	it('highlights and scrolls to the focused approval card', async () => {
		const scrollSpy = vi
			.spyOn(Element.prototype, 'scrollIntoView')
			.mockImplementation(() => {});
		window.history.replaceState({}, '', '/approvals?focus=act-focus-1');
		setupWatcher([
			{
				id: 'act-other',
				kind: 'action' as const,
				action_name: 'noop',
				requested_at: '2026-05-04T12:00:00Z'
			},
			{
				id: 'act-focus-1',
				kind: 'action' as const,
				action_name: 'send-email',
				requested_at: '2026-05-04T12:00:01Z'
			}
		]);

		render(Page);

		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});

		const cards = await screen.findAllByTestId('action-approval-card');
		const focused = cards.find(
			(c) => c.getAttribute('data-approval-id') === 'act-focus-1'
		);
		const other = cards.find(
			(c) => c.getAttribute('data-approval-id') === 'act-other'
		);
		expect(focused).toBeDefined();
		expect(other).toBeDefined();
		expect(focused!.getAttribute('data-focused')).toBe('true');
		expect(other!.getAttribute('data-focused')).toBe('false');
		expect(focused!.className).toContain('ring-2');
		expect(other!.className).not.toContain('ring-2 ring-primary');
		await waitFor(() => {
			expect(scrollSpy).toHaveBeenCalled();
		});

		scrollSpy.mockRestore();
	});

	it('shows a banner when the focused id is not in the pending list', async () => {
		window.history.replaceState({}, '', '/approvals?focus=act-missing');
		setupWatcher([
			{
				id: 'act-other',
				kind: 'action' as const,
				action_name: 'send-email',
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('focused-missing-banner')).toBeInTheDocument();
		});
		expect(screen.getByTestId('focused-missing-banner').textContent).toContain(
			'act-missing'
		);
	});

	it('shows the missing banner alongside the empty-state hint when no approvals are pending', async () => {
		window.history.replaceState({}, '', '/approvals?focus=act-gone');

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('focused-missing-banner')).toBeInTheDocument();
		});
		expect(
			screen.getByText(/No pending approvals\. The agent's blocked tool calls/)
		).toBeInTheDocument();
	});

	it('clears the missing banner once the focused approval arrives via SSE', async () => {
		window.history.replaceState({}, '', '/approvals?focus=act-late');

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('focused-missing-banner')).toBeInTheDocument();
		});

		captured!.onPending({
			id: 'act-late',
			kind: 'action' as const,
			action_name: 'send-email',
			requested_at: '2026-05-04T12:00:05Z'
		});

		await waitFor(() => {
			expect(
				screen.queryByTestId('focused-missing-banner')
			).not.toBeInTheDocument();
		});
		const card = (await screen.findAllByTestId('action-approval-card')).find(
			(c) => c.getAttribute('data-approval-id') === 'act-late'
		);
		expect(card!.getAttribute('data-focused')).toBe('true');
	});

	it('does not highlight anything when no ?focus param is set', async () => {
		setupWatcher([
			{
				id: 'act-1',
				kind: 'action' as const,
				action_name: 'send-email',
				requested_at: '2026-05-04T12:00:00Z'
			}
		]);

		render(Page);

		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});

		const card = screen.getByTestId('action-approval-card');
		expect(card.getAttribute('data-focused')).toBe('false');
		expect(
			screen.queryByTestId('focused-missing-banner')
		).not.toBeInTheDocument();
	});
});

