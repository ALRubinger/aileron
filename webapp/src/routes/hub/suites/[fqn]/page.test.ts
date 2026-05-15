import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	getHubSuite: vi.fn(),
	getHubInstallDecision: vi.fn(),
	installConnector: vi.fn()
}));

vi.mock('$app/state', () => ({
	page: { params: { fqn: 'github://alice/conn-google/suite' } }
}));

vi.mock('$app/navigation', () => ({
	goto: vi.fn()
}));

import Page from './+page.svelte';
import { getHubSuite, type HubSuiteEntry } from '$lib/api';

const gmailSuite: HubSuiteEntry = {
	fqn: 'github://alice/conn-google/suite',
	description: 'Read and draft Gmail; read and create calendar events',
	publisher_github: 'alice',
	member_actions: [
		'github://alice/conn-google/actions/list-recent-emails',
		'github://alice/conn-google/actions/draft-email'
	],
	connectors_required: ['github://alice/conn-google'],
	category: 'communication'
};

beforeEach(() => {
	vi.mocked(getHubSuite).mockReset();
});

describe('Hub suite detail page', () => {
	it('fetches the suite by FQN and renders metadata', async () => {
		vi.mocked(getHubSuite).mockResolvedValue(gmailSuite);
		render(Page);
		await waitFor(() => {
			expect(getHubSuite).toHaveBeenCalledWith(gmailSuite.fqn);
		});
		expect(screen.getByText(gmailSuite.fqn)).toBeInTheDocument();
		expect(screen.getByText(gmailSuite.description)).toBeInTheDocument();
		expect(screen.getByText('alice')).toBeInTheDocument();
		expect(screen.getByText('communication')).toBeInTheDocument();
	});

	it('renders each member action as a link to the action detail', async () => {
		vi.mocked(getHubSuite).mockResolvedValue(gmailSuite);
		render(Page);
		await waitFor(() => {
			expect(getHubSuite).toHaveBeenCalled();
		});
		const members = screen.getByTestId('hub-detail-members');
		for (const action of gmailSuite.member_actions) {
			const link = members.querySelector(`a[href="/hub/actions/${encodeURIComponent(action)}"]`);
			expect(link, `expected member-action link for ${action}`).not.toBeNull();
		}
	});

	it('renders the connectors_required list when present', async () => {
		vi.mocked(getHubSuite).mockResolvedValue(gmailSuite);
		render(Page);
		await waitFor(() => {
			expect(getHubSuite).toHaveBeenCalled();
		});
		const list = screen.getByTestId('hub-detail-connectors');
		expect(list).toHaveTextContent('github://alice/conn-google');
	});

	it('omits the connectors_required block when the suite does not declare one', async () => {
		const minimalSuite: HubSuiteEntry = {
			fqn: 'github://alice/x/suite',
			description: 'minimal',
			publisher_github: 'alice',
			member_actions: ['github://alice/x/actions/foo']
			// no connectors_required
		};
		vi.mocked(getHubSuite).mockResolvedValue(minimalSuite);
		render(Page);
		await waitFor(() => {
			expect(getHubSuite).toHaveBeenCalled();
		});
		expect(screen.queryByTestId('hub-detail-connectors')).toBeNull();
	});

	it('surfaces a fetch error rather than rendering a partial UI', async () => {
		vi.mocked(getHubSuite).mockRejectedValue(new Error('not_found'));
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('hub-detail-error')).toHaveTextContent(
				'not_found'
			);
		});
	});
});
