import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	listHubConnectors: vi.fn(),
	getHubInstallDecision: vi.fn(),
	installConnector: vi.fn()
}));

import Page from './+page.svelte';
import {
	listHubConnectors,
	getHubInstallDecision,
	type HubConnectorEntry,
	type HubInstallDecision
} from '$lib/api';

const aliceEntry: HubConnectorEntry = {
	fqn: 'github://alice/calendar',
	description: 'Google Calendar connector',
	publisher_github: 'alice',
	key_url: 'https://example.com/alice.pub',
	release_pattern: 'v*'
};
const bobEntry: HubConnectorEntry = {
	fqn: 'github://bob/slack',
	description: 'Slack messaging connector',
	publisher_github: 'bob',
	key_url: 'https://example.com/bob.pub',
	release_pattern: 'v*'
};

const aliceDecision: HubInstallDecision = {
	fqn: aliceEntry.fqn,
	description: aliceEntry.description,
	publisher_github: 'alice',
	fingerprint: 'sha256:aliceFingerprintAAAAAAA',
	trust_state: 'unknown',
	publisher_footprint: [],
	risk_indicators: ["First connector by this publisher you've installed"]
};

beforeEach(() => {
	vi.mocked(listHubConnectors).mockReset();
	vi.mocked(getHubInstallDecision).mockReset();
});

describe('Hub page — list + search', () => {
	it('renders an empty-state hint when the Hub is empty', async () => {
		vi.mocked(listHubConnectors).mockResolvedValue({ connectors: [] });
		render(Page);
		await waitFor(() => {
			expect(
				screen.getByText(/No connectors published to the Hub/)
			).toBeInTheDocument();
		});
	});

	it('renders Hub entries returned by the API', async () => {
		vi.mocked(listHubConnectors).mockResolvedValue({
			connectors: [aliceEntry, bobEntry]
		});
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('github://alice/calendar')).toBeInTheDocument();
			expect(screen.getByText('github://bob/slack')).toBeInTheDocument();
			expect(screen.getByText('Google Calendar connector')).toBeInTheDocument();
		});
		expect(listHubConnectors).toHaveBeenCalledWith('');
	});

	it('surfaces an API error rather than silently rendering empty', async () => {
		vi.mocked(listHubConnectors).mockRejectedValue(new Error('hub_unreachable'));
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('hub-list-error')).toHaveTextContent(
				'hub_unreachable'
			);
		});
	});

	it('debounces search input and re-fetches with the query parameter', async () => {
		vi.useFakeTimers();
		vi.mocked(listHubConnectors).mockResolvedValue({ connectors: [aliceEntry] });
		render(Page);
		await vi.waitFor(() => {
			expect(listHubConnectors).toHaveBeenCalledWith('');
		});
		const input = screen.getByTestId('hub-search-input');
		await fireEvent.input(input, { target: { value: 'calendar' } });
		// Debounce window is 250ms — advance the timer and let promises settle.
		await vi.advanceTimersByTimeAsync(300);
		expect(listHubConnectors).toHaveBeenLastCalledWith('calendar');
		vi.useRealTimers();
	});

	it('echoes the query in the no-match empty state', async () => {
		vi.useFakeTimers();
		vi.mocked(listHubConnectors).mockResolvedValue({ connectors: [] });
		render(Page);
		await vi.waitFor(() => {
			expect(listHubConnectors).toHaveBeenCalled();
		});
		await fireEvent.input(screen.getByTestId('hub-search-input'), {
			target: { value: 'zzz' }
		});
		await vi.advanceTimersByTimeAsync(300);
		await vi.waitFor(() => {
			expect(screen.getByTestId('hub-empty-state')).toHaveTextContent('"zzz"');
		});
		vi.useRealTimers();
	});
});

describe('Hub page — install modal integration', () => {
	it('opens the install modal with the clicked entry’s install-decision', async () => {
		vi.mocked(listHubConnectors).mockResolvedValue({
			connectors: [aliceEntry, bobEntry]
		});
		vi.mocked(getHubInstallDecision).mockResolvedValue(aliceDecision);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('github://alice/calendar')).toBeInTheDocument();
		});
		const aliceCard = screen
			.getAllByTestId('hub-entry-card')
			.find((c) => c.getAttribute('data-fqn') === aliceEntry.fqn);
		expect(aliceCard).toBeDefined();
		const installBtn = aliceCard!.querySelector(
			'[data-testid="hub-install-cta"]'
		) as HTMLButtonElement;
		await fireEvent.click(installBtn);
		await waitFor(() => {
			expect(getHubInstallDecision).toHaveBeenCalledWith(aliceEntry.fqn);
		});
		await waitFor(() => {
			expect(screen.getByTestId('hub-trust-state')).toHaveAttribute(
				'data-trust-state',
				'unknown'
			);
		});
	});
});
