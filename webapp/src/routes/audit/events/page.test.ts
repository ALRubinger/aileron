import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	listAudit: vi.fn()
}));

import Page from './+page.svelte';
import { listAudit, type AuditEvent } from '$lib/api';

// --- fixtures ---

function event(opts: {
	id: string;
	type: string;
	timestamp?: string;
	payload?: Record<string, unknown>;
}): AuditEvent {
	return {
		audit_id: opts.id,
		event_type: opts.type,
		timestamp: opts.timestamp ?? '2026-07-04T00:00:00Z',
		payload: opts.payload ?? {}
	};
}

beforeEach(() => {
	vi.mocked(listAudit).mockReset();
});

describe('Audit events feed — /audit/events', () => {
	it('lists every event class newest-first with a CLI-parity summary line', async () => {
		vi.mocked(listAudit).mockResolvedValue([
			event({
				id: 'evt-approval',
				type: 'approval.approved',
				payload: {
					'aileron.action.name': 'send_email',
					'aileron.connector.fqn': 'github://aileron/google'
				}
			}),
			event({
				id: 'evt-binding',
				type: 'binding.created',
				payload: { 'aileron.connector.fqn': 'github://aileron/slack' }
			}),
			event({
				id: 'evt-fail',
				type: 'execution.failed',
				payload: { 'aileron.failure.class': 'timeout' }
			})
		]);

		render(Page);

		await waitFor(() => {
			expect(screen.getByTestId('events-list')).toBeInTheDocument();
		});
		// listAudit is called with no filters — the general feed.
		expect(vi.mocked(listAudit)).toHaveBeenCalledWith();

		const rows = screen.getAllByTestId('event-row');
		expect(rows.length).toBe(3);

		// The approval row renders the event type, its audit id, and the
		// name+connector summary the CLI would print for the same event.
		const approvalRow = rows[0];
		expect(approvalRow).toHaveTextContent('approval.approved');
		expect(approvalRow).toHaveTextContent('evt-approval');
		expect(approvalRow).toHaveTextContent('name=send_email connector=github://aileron/google');

		// A binding event summarizes as connector only; a failure as class.
		expect(rows[1]).toHaveTextContent('connector=github://aileron/slack');
		expect(rows[2]).toHaveTextContent('class=timeout');
	});

	it('omits the summary line for an event with no identifying payload', async () => {
		vi.mocked(listAudit).mockResolvedValue([event({ id: 'evt-bare', type: 'daemon.started' })]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('event-row')).toBeInTheDocument();
		});
		expect(screen.queryByTestId('event-summary')).not.toBeInTheDocument();
	});

	it('shows the empty state when nothing is recorded', async () => {
		vi.mocked(listAudit).mockResolvedValue([]);
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('events-empty')).toBeInTheDocument();
		});
	});

	it('surfaces a listing error', async () => {
		vi.mocked(listAudit).mockRejectedValue(new Error('audit down'));
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('events-error')).toHaveTextContent('audit down');
		});
	});

	it('re-fetches on refresh', async () => {
		vi.mocked(listAudit).mockResolvedValue([event({ id: 'evt-1', type: 'action.installed' })]);
		render(Page);
		await screen.findByTestId('events-list');
		expect(vi.mocked(listAudit)).toHaveBeenCalledTimes(1);
		await fireEvent.click(screen.getByTestId('events-refresh'));
		await waitFor(() => {
			expect(vi.mocked(listAudit)).toHaveBeenCalledTimes(2);
		});
	});

	it('keeps refresh available after an empty result so the feed can recover', async () => {
		vi.mocked(listAudit).mockResolvedValueOnce([]);
		render(Page);
		await screen.findByTestId('events-empty');
		// Refresh lives outside the state branches, so it is present even
		// when the feed rendered empty.
		vi.mocked(listAudit).mockResolvedValueOnce([event({ id: 'evt-1', type: 'action.installed' })]);
		await fireEvent.click(screen.getByTestId('events-refresh'));
		await waitFor(() => {
			expect(screen.getByTestId('events-list')).toBeInTheDocument();
		});
	});

	it('links back to the artifact-scoped provenance view', async () => {
		vi.mocked(listAudit).mockResolvedValue([]);
		render(Page);
		await screen.findByTestId('events-empty');
		expect(screen.getByTestId('to-provenance')).toHaveAttribute('href', '/audit');
	});
});
