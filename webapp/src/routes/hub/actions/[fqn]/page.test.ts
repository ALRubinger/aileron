import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	getHubAction: vi.fn(),
	getHubInstallDecision: vi.fn(),
	installConnector: vi.fn()
}));

vi.mock('$app/state', () => ({
	page: { params: { fqn: 'github://alice/conn-google/actions/draft-email' } }
}));

vi.mock('$app/navigation', () => ({
	goto: vi.fn()
}));

import Page from './+page.svelte';
import { getHubAction, type HubActionEntry } from '$lib/api';

const draftEmail: HubActionEntry = {
	fqn: 'github://alice/conn-google/actions/draft-email',
	description: 'Draft a Gmail message with subject, recipients, and body',
	publisher_github: 'alice',
	connector_fqn: 'github://alice/conn-google',
	intents: ['draft email', 'compose gmail'],
	category: 'communication'
};

beforeEach(() => {
	vi.mocked(getHubAction).mockReset();
});

describe('Hub action detail page', () => {
	it('fetches the action by FQN and renders metadata', async () => {
		vi.mocked(getHubAction).mockResolvedValue(draftEmail);
		render(Page);
		await waitFor(() => {
			expect(getHubAction).toHaveBeenCalledWith(draftEmail.fqn);
		});
		expect(screen.getByText(draftEmail.fqn)).toBeInTheDocument();
		expect(screen.getByText(draftEmail.description)).toBeInTheDocument();
		expect(screen.getByText('alice')).toBeInTheDocument();
		expect(screen.getByText(draftEmail.connector_fqn)).toBeInTheDocument();
		expect(screen.getByText('communication')).toBeInTheDocument();
		// Intents render as a list.
		const intents = screen.getByTestId('hub-detail-intents');
		expect(intents).toHaveTextContent('draft email');
		expect(intents).toHaveTextContent('compose gmail');
	});

	it('renders the connector dependency as a link to the connector detail', async () => {
		vi.mocked(getHubAction).mockResolvedValue(draftEmail);
		render(Page);
		await waitFor(() => {
			expect(getHubAction).toHaveBeenCalled();
		});
		const link = screen.getByTestId('hub-detail-connector-link') as HTMLAnchorElement;
		expect(link).toHaveAttribute(
			'href',
			'/hub/connectors/' + encodeURIComponent(draftEmail.connector_fqn)
		);
	});

	it('surfaces a fetch error rather than rendering a partial UI', async () => {
		vi.mocked(getHubAction).mockRejectedValue(new Error('not_found'));
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('hub-detail-error')).toHaveTextContent(
				'not_found'
			);
		});
	});

	it('does not render the install CTA before the entry has loaded', async () => {
		// Reject so the entry never loads — the install CTA must not
		// render because the modal requires entry.fqn to target.
		vi.mocked(getHubAction).mockRejectedValue(new Error('boom'));
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('hub-detail-error')).toBeInTheDocument();
		});
		expect(screen.queryByTestId('hub-detail-install-cta')).toBeNull();
	});
});
