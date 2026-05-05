import { describe, it, expect, vi, beforeEach } from 'vitest';
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
