import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	listHubConnectors: vi.fn(),
	listHubActions: vi.fn(),
	listHubSuites: vi.fn(),
	getHubInstallDecision: vi.fn(),
	installConnector: vi.fn()
}));

import Page from './+page.svelte';
import {
	listHubConnectors,
	listHubActions,
	listHubSuites,
	getHubInstallDecision,
	type HubConnectorEntry,
	type HubActionEntry,
	type HubSuiteEntry,
	type HubInstallDecision
} from '$lib/api';

const aliceConnector: HubConnectorEntry = {
	fqn: 'github://alice/calendar',
	description: 'Google Calendar connector',
	publisher_github: 'alice',
	key_url: 'https://example.com/alice.pub',
	release_pattern: 'v*'
};
const bobConnector: HubConnectorEntry = {
	fqn: 'github://bob/slack',
	description: 'Slack messaging connector',
	publisher_github: 'bob',
	key_url: 'https://example.com/bob.pub',
	release_pattern: 'v*'
};

const draftEmailAction: HubActionEntry = {
	fqn: 'github://alice/conn-google/actions/draft-email',
	description: 'Draft a Gmail message with subject, recipients, and body',
	publisher_github: 'alice',
	connector_fqn: 'github://alice/conn-google',
	intents: ['draft email', 'compose gmail'],
	category: 'communication'
};

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

const aliceDecision: HubInstallDecision = {
	fqn: aliceConnector.fqn,
	description: aliceConnector.description,
	publisher_github: 'alice',
	fingerprint: 'sha256:aliceFingerprintAAAAAAA',
	trust_state: 'unknown',
	publisher_footprint: [],
	risk_indicators: ["First connector by this publisher you've installed"]
};

beforeEach(() => {
	vi.mocked(listHubConnectors).mockReset();
	vi.mocked(listHubActions).mockReset();
	vi.mocked(listHubSuites).mockReset();
	vi.mocked(getHubInstallDecision).mockReset();
	// Sensible defaults: every tab returns empty unless overridden by
	// the specific test. Avoids "function returned undefined" failures
	// when a tab-switch test triggers a fetch the test didn't author.
	vi.mocked(listHubConnectors).mockResolvedValue({ connectors: [] });
	vi.mocked(listHubActions).mockResolvedValue({ actions: [] });
	vi.mocked(listHubSuites).mockResolvedValue({ suites: [] });
});

describe('Hub page — tab shell', () => {
	it('lands on the Suites tab and queries /hub/suites first', async () => {
		render(Page);
		await waitFor(() => {
			expect(listHubSuites).toHaveBeenCalledWith(undefined);
		});
		// Suites tab is the default per the action-first IA.
		expect(screen.getByTestId('hub-tab-suites')).toHaveAttribute(
			'aria-selected',
			'true'
		);
	});

	it('renders Suite entries with action count and category', async () => {
		vi.mocked(listHubSuites).mockResolvedValue({ suites: [gmailSuite] });
		render(Page);
		await waitFor(() => {
			expect(screen.getByText(gmailSuite.fqn)).toBeInTheDocument();
			expect(screen.getByText(gmailSuite.description)).toBeInTheDocument();
			expect(screen.getByText(/Actions: 2/)).toBeInTheDocument();
			expect(screen.getByText(/Category: communication/)).toBeInTheDocument();
		});
	});

	it('switches to the Actions tab and fetches /hub/actions', async () => {
		vi.mocked(listHubActions).mockResolvedValue({ actions: [draftEmailAction] });
		render(Page);
		await waitFor(() => {
			expect(listHubSuites).toHaveBeenCalled();
		});
		await fireEvent.click(screen.getByTestId('hub-tab-actions'));
		await waitFor(() => {
			expect(listHubActions).toHaveBeenCalledWith(undefined);
		});
		expect(screen.getByTestId('hub-tab-actions')).toHaveAttribute(
			'aria-selected',
			'true'
		);
		expect(screen.getByText(draftEmailAction.fqn)).toBeInTheDocument();
		expect(screen.getByText(/draft email/)).toBeInTheDocument();
		expect(screen.getByText(/compose gmail/)).toBeInTheDocument();
		expect(screen.getByText(/Requires:/)).toBeInTheDocument();
	});

	it('switches to the Providers tab and fetches /hub/connectors', async () => {
		vi.mocked(listHubConnectors).mockResolvedValue({
			connectors: [aliceConnector, bobConnector]
		});
		render(Page);
		await waitFor(() => {
			expect(listHubSuites).toHaveBeenCalled();
		});
		await fireEvent.click(screen.getByTestId('hub-tab-providers'));
		await waitFor(() => {
			expect(listHubConnectors).toHaveBeenCalledWith(undefined);
		});
		expect(screen.getByText('github://alice/calendar')).toBeInTheDocument();
		expect(screen.getByText('github://bob/slack')).toBeInTheDocument();
	});

	it('clicking the active tab does not refetch', async () => {
		render(Page);
		await waitFor(() => {
			expect(listHubSuites).toHaveBeenCalledTimes(1);
		});
		await fireEvent.click(screen.getByTestId('hub-tab-suites'));
		// One additional event loop tick.
		await Promise.resolve();
		expect(listHubSuites).toHaveBeenCalledTimes(1);
	});
});

describe('Hub page — Providers tab (legacy connector flow)', () => {
	it('renders an empty-state hint when no connectors are published', async () => {
		render(Page);
		await waitFor(() => {
			expect(listHubSuites).toHaveBeenCalled();
		});
		await fireEvent.click(screen.getByTestId('hub-tab-providers'));
		await waitFor(() => {
			expect(
				screen.getByText(/No connectors published to the Hub/)
			).toBeInTheDocument();
		});
	});

	it('renders connector entries returned by the API', async () => {
		vi.mocked(listHubConnectors).mockResolvedValue({
			connectors: [aliceConnector, bobConnector]
		});
		render(Page);
		await waitFor(() => {
			expect(listHubSuites).toHaveBeenCalled();
		});
		await fireEvent.click(screen.getByTestId('hub-tab-providers'));
		await waitFor(() => {
			expect(screen.getByText('github://alice/calendar')).toBeInTheDocument();
			expect(screen.getByText('github://bob/slack')).toBeInTheDocument();
			expect(screen.getByText('Google Calendar connector')).toBeInTheDocument();
		});
	});

	it('surfaces an API error rather than silently rendering empty', async () => {
		vi.mocked(listHubConnectors).mockRejectedValue(new Error('hub_unreachable'));
		render(Page);
		await waitFor(() => {
			expect(listHubSuites).toHaveBeenCalled();
		});
		await fireEvent.click(screen.getByTestId('hub-tab-providers'));
		await waitFor(() => {
			expect(screen.getByTestId('hub-list-error')).toHaveTextContent(
				'hub_unreachable'
			);
		});
	});
});

describe('Hub page — search (per-tab)', () => {
	it('debounces search input and re-fetches with the query parameter on the active tab', async () => {
		vi.useFakeTimers();
		vi.mocked(listHubSuites).mockResolvedValue({ suites: [gmailSuite] });
		render(Page);
		await vi.waitFor(() => {
			expect(listHubSuites).toHaveBeenCalledWith(undefined);
		});
		const input = screen.getByTestId('hub-search-input');
		await fireEvent.input(input, { target: { value: 'calendar' } });
		// Debounce window is 250ms.
		await vi.advanceTimersByTimeAsync(300);
		expect(listHubSuites).toHaveBeenLastCalledWith('calendar');
		vi.useRealTimers();
	});

	it('echoes the query in the no-match empty state on every tab', async () => {
		vi.useFakeTimers();
		render(Page);
		await vi.waitFor(() => {
			expect(listHubSuites).toHaveBeenCalled();
		});
		// Default tab (Suites): empty + query echo.
		await fireEvent.input(screen.getByTestId('hub-search-input'), {
			target: { value: 'zzz' }
		});
		await vi.advanceTimersByTimeAsync(300);
		await vi.waitFor(() => {
			expect(screen.getByTestId('hub-empty-state')).toHaveTextContent('"zzz"');
		});
		// Switch tabs (search persists; re-fetches on the new tab).
		await fireEvent.click(screen.getByTestId('hub-tab-providers'));
		await vi.waitFor(() => {
			expect(listHubConnectors).toHaveBeenCalledWith('zzz');
		});
		await vi.waitFor(() => {
			expect(screen.getByTestId('hub-empty-state')).toHaveTextContent('"zzz"');
		});
		vi.useRealTimers();
	});
});

describe('Hub page — install modal integration', () => {
	it('opens the install modal with the clicked entry’s install-decision', async () => {
		vi.mocked(listHubConnectors).mockResolvedValue({
			connectors: [aliceConnector, bobConnector]
		});
		vi.mocked(getHubInstallDecision).mockResolvedValue(aliceDecision);
		render(Page);
		await waitFor(() => {
			expect(listHubSuites).toHaveBeenCalled();
		});
		await fireEvent.click(screen.getByTestId('hub-tab-providers'));
		await waitFor(() => {
			expect(screen.getByText('github://alice/calendar')).toBeInTheDocument();
		});
		const aliceCard = screen
			.getAllByTestId('hub-entry-card')
			.find((c) => c.getAttribute('data-fqn') === aliceConnector.fqn);
		expect(aliceCard).toBeDefined();
		const installBtn = aliceCard!.querySelector(
			'[data-testid="hub-install-cta"]'
		) as HTMLButtonElement;
		await fireEvent.click(installBtn);
		await waitFor(() => {
			expect(getHubInstallDecision).toHaveBeenCalledWith(aliceConnector.fqn);
		});
		await waitFor(() => {
			expect(screen.getByTestId('hub-trust-state')).toHaveAttribute(
				'data-trust-state',
				'unknown'
			);
		});
	});
});
