import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	listApprovals: vi.fn(),
	listActionApprovals: vi.fn(),
	decideActionApproval: vi.fn()
}));

import Page from './+page.svelte';
import { listApprovals, listActionApprovals, decideActionApproval } from '$lib/api';

beforeEach(() => {
	vi.mocked(listApprovals).mockResolvedValue({ items: [] });
	vi.mocked(listActionApprovals).mockResolvedValue({ items: [] });
	vi.mocked(decideActionApproval).mockResolvedValue(null);
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
	it('renders pending action approvals with action name, connector, and args', async () => {
		vi.mocked(listActionApprovals).mockResolvedValue({
			items: [
				{
					id: 'act-test-1',
					action_name: 'send-email',
					connector_fqn: 'github://x/aileron-connector-google',
					args: { to: 'alice@example.com', subject: 'hi' },
					session_id: 'session-42',
					requested_at: '2026-05-04T12:00:00Z'
				}
			]
		});

		render(Page);

		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});
		expect(screen.getByText(/github:\/\/x\/aileron-connector-google/)).toBeInTheDocument();
		// Args are rendered as JSON; recipient and subject must both appear in
		// the rendered <pre> so the user can review what would be sent.
		const argsBlock = screen.getByTestId('approval-args');
		expect(argsBlock.textContent).toContain('alice@example.com');
		expect(argsBlock.textContent).toContain('subject');
		// Session id is surfaced verbatim.
		expect(screen.getByText(/Session: session-42/)).toBeInTheDocument();
	});

	it('does not render the args block when an entry has no args', async () => {
		vi.mocked(listActionApprovals).mockResolvedValue({
			items: [
				{
					id: 'act-test-1',
					action_name: 'noop',
					requested_at: '2026-05-04T12:00:00Z'
				}
			]
		});
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('noop')).toBeInTheDocument();
		});
		// `(no args)` placeholder keeps the layout stable rather than
		// collapsing the args row entirely — easier to scan a list.
		expect(screen.getByTestId('approval-args').textContent).toBe('(no args)');
	});

	it('approves an action and removes it from the list optimistically', async () => {
		vi.mocked(listActionApprovals).mockResolvedValue({
			items: [
				{
					id: 'act-test-1',
					action_name: 'send-email',
					requested_at: '2026-05-04T12:00:00Z'
				}
			]
		});

		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});

		const approveButton = screen.getByTestId('approve-button');
		await fireEvent.click(approveButton);

		await waitFor(() => {
			expect(decideActionApproval).toHaveBeenCalledWith('act-test-1', true, '');
		});
		// After resolution, the card disappears even before the next poll
		// — this is the optimistic-update path the user perceives as
		// instant feedback. Reconciliation happens on the next interval tick.
		await waitFor(() => {
			expect(screen.queryByText('send-email')).not.toBeInTheDocument();
		});
	});

	it('denies an action with the typed reason', async () => {
		vi.mocked(listActionApprovals).mockResolvedValue({
			items: [
				{
					id: 'act-test-1',
					action_name: 'send-email',
					requested_at: '2026-05-04T12:00:00Z'
				}
			]
		});

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
				'wrong recipient'
			);
		});
	});

	it('surfaces server errors from decideActionApproval', async () => {
		vi.mocked(listActionApprovals).mockResolvedValue({
			items: [
				{
					id: 'act-test-1',
					action_name: 'send-email',
					requested_at: '2026-05-04T12:00:00Z'
				}
			]
		});
		vi.mocked(decideActionApproval).mockRejectedValue(
			new Error('approval is unknown or already resolved')
		);

		render(Page);
		await waitFor(() => {
			expect(screen.getByText('send-email')).toBeInTheDocument();
		});

		await fireEvent.click(screen.getByTestId('approve-button'));

		await waitFor(() => {
			expect(
				screen.getByText('approval is unknown or already resolved')
			).toBeInTheDocument();
		});
	});
});
