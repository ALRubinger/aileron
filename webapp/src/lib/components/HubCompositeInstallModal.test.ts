import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';

vi.mock('$lib/api', () => ({
	getHubActionInstallDecision: vi.fn(),
	getHubSuiteInstallDecision: vi.fn(),
	installAction: vi.fn()
}));

import Modal from './HubCompositeInstallModal.svelte';
import {
	getHubActionInstallDecision,
	getHubSuiteInstallDecision,
	installAction,
	type HubActionInstallDecision,
	type HubSuiteInstallDecision,
	type InstalledActionResult
} from '$lib/api';

const actionDecision: HubActionInstallDecision = {
	kind: 'action',
	fqn: 'github://alice/conn-google/actions/draft-email',
	description: 'Draft a Gmail message',
	publisher_github: 'alice',
	connector_fqn: 'github://alice/conn-google',
	authorities: [
		{
			fqn: 'github://alice/conn-google',
			publisher_github: 'alice',
			fingerprint: 'sha256:aliceFingerprintAAAAAA',
			trust_state: 'unknown',
			publisher_footprint: ['github://alice/other-conn'],
			risk_indicators: ["First connector by this publisher you've installed"]
		}
	],
	latest_version: '1.2.0'
};

const suiteDecision: HubSuiteInstallDecision = {
	kind: 'suite',
	fqn: 'github://alice/conn-google/suite',
	description: 'Gmail and Calendar bundle',
	publisher_github: 'alice',
	member_actions: [
		'github://alice/conn-google/actions/list-recent-emails',
		'github://alice/conn-google/actions/draft-email'
	],
	authorities: [
		{
			fqn: 'github://alice/conn-google',
			publisher_github: 'alice',
			fingerprint: 'sha256:aliceFingerprintAAAAAA',
			trust_state: 'unknown',
			publisher_footprint: [],
			risk_indicators: ['First connector by this publisher']
		},
		{
			fqn: 'github://bob/conn-calendar',
			publisher_github: 'bob',
			fingerprint: 'sha256:bobFingerprintBBBBBBB',
			trust_state: 'already_trusted',
			publisher_footprint: [],
			risk_indicators: []
		}
	],
	latest_version: '2.1.0'
};

beforeEach(() => {
	vi.mocked(getHubActionInstallDecision).mockReset();
	vi.mocked(getHubSuiteInstallDecision).mockReset();
	vi.mocked(installAction).mockReset();
});

describe('HubCompositeInstallModal — action', () => {
	it('fetches the action composite payload and renders the trust panel', async () => {
		vi.mocked(getHubActionInstallDecision).mockResolvedValue(actionDecision);
		render(Modal, {
			props: {
				fqn: actionDecision.fqn,
				kind: 'action',
				onClose: () => {}
			}
		});
		await waitFor(() => {
			expect(getHubActionInstallDecision).toHaveBeenCalledWith(actionDecision.fqn);
		});
		await waitFor(() => {
			expect(screen.getByText(actionDecision.fqn)).toBeInTheDocument();
		});
		expect(screen.getByText(actionDecision.description)).toBeInTheDocument();
		// connector_fqn surfaces in two places: the action-level
		// "Connector" row and the authority panel's FQN header.
		expect(screen.getAllByText(actionDecision.connector_fqn).length).toBeGreaterThanOrEqual(1);
		// One authority surfaced with its fingerprint + trust badge.
		const authorities = screen.getAllByTestId('hub-composite-authority');
		expect(authorities).toHaveLength(1);
		expect(authorities[0]).toHaveAttribute('data-fqn', 'github://alice/conn-google');
		expect(screen.getByText('sha256:aliceFingerprintAAAAAA')).toBeInTheDocument();
		expect(screen.getByTestId('hub-composite-trust-state')).toHaveAttribute(
			'data-trust-state',
			'unknown'
		);
	});

	it('renders the CLI command for completing the install', async () => {
		vi.mocked(getHubActionInstallDecision).mockResolvedValue(actionDecision);
		render(Modal, {
			props: { fqn: actionDecision.fqn, kind: 'action', onClose: () => {} }
		});
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-cli-command')).toBeInTheDocument();
		});
		expect(screen.getByTestId('hub-composite-cli-command')).toHaveTextContent(
			`aileron action add ${actionDecision.fqn}@latest`
		);
	});

	it('surfaces fetch errors instead of rendering an empty panel', async () => {
		vi.mocked(getHubActionInstallDecision).mockRejectedValue(
			new Error('not_found')
		);
		render(Modal, {
			props: { fqn: actionDecision.fqn, kind: 'action', onClose: () => {} }
		});
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-decision-error')).toHaveTextContent(
				'not_found'
			);
		});
	});

	it('renders publisher_footprint when verbose-ish info is available', async () => {
		vi.mocked(getHubActionInstallDecision).mockResolvedValue(actionDecision);
		render(Modal, {
			props: { fqn: actionDecision.fqn, kind: 'action', onClose: () => {} }
		});
		await waitFor(() => {
			expect(screen.getByText('github://alice/other-conn')).toBeInTheDocument();
		});
	});
});

describe('HubCompositeInstallModal — suite', () => {
	it('renders one panel per unique connector authority', async () => {
		vi.mocked(getHubSuiteInstallDecision).mockResolvedValue(suiteDecision);
		render(Modal, {
			props: {
				fqn: suiteDecision.fqn,
				kind: 'suite',
				onClose: () => {}
			}
		});
		await waitFor(() => {
			expect(getHubSuiteInstallDecision).toHaveBeenCalledWith(suiteDecision.fqn);
		});
		await waitFor(() => {
			expect(screen.getByText(suiteDecision.fqn)).toBeInTheDocument();
		});
		const authorities = screen.getAllByTestId('hub-composite-authority');
		expect(authorities).toHaveLength(2);
		const fqns = authorities.map((a) => a.getAttribute('data-fqn'));
		expect(fqns).toEqual(['github://alice/conn-google', 'github://bob/conn-calendar']);
		// Per-authority trust state — one unknown (alice), one
		// already_trusted (bob).
		const trustBadges = screen.getAllByTestId('hub-composite-trust-state');
		expect(trustBadges[0]).toHaveAttribute('data-trust-state', 'unknown');
		expect(trustBadges[1]).toHaveAttribute('data-trust-state', 'already_trusted');
	});

	it('renders the member-action list', async () => {
		vi.mocked(getHubSuiteInstallDecision).mockResolvedValue(suiteDecision);
		render(Modal, {
			props: { fqn: suiteDecision.fqn, kind: 'suite', onClose: () => {} }
		});
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-members')).toBeInTheDocument();
		});
		const members = screen.getByTestId('hub-composite-members');
		for (const a of suiteDecision.member_actions) {
			expect(members).toHaveTextContent(a);
		}
	});

	it('renders the CLI command for the suite install path', async () => {
		vi.mocked(getHubSuiteInstallDecision).mockResolvedValue(suiteDecision);
		render(Modal, {
			props: { fqn: suiteDecision.fqn, kind: 'suite', onClose: () => {} }
		});
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-cli-command')).toBeInTheDocument();
		});
		// Hub suite FQN is `<owner>/<repo>/suite`; the CLI's add-suite
		// command takes the `.toml` file path, so the modal appends
		// `.toml@latest` to bridge the gap.
		expect(screen.getByTestId('hub-composite-cli-command')).toHaveTextContent(
			`aileron action add-suite ${suiteDecision.fqn}.toml@latest`
		);
	});
});

describe('HubCompositeInstallModal — dispatch', () => {
	it('does not fetch when fqn is null', () => {
		render(Modal, { props: { fqn: null, kind: null, onClose: () => {} } });
		expect(getHubActionInstallDecision).not.toHaveBeenCalled();
		expect(getHubSuiteInstallDecision).not.toHaveBeenCalled();
	});

	it('does not fetch when kind is null', () => {
		render(Modal, {
			props: { fqn: 'github://x/y/actions/z', kind: null, onClose: () => {} }
		});
		expect(getHubActionInstallDecision).not.toHaveBeenCalled();
		expect(getHubSuiteInstallDecision).not.toHaveBeenCalled();
	});
});

// --- #743: webapp install execution ---

// Mirrors the daemon's /v1/actions/install response shape that lands
// on the success panel. Builds enough fields for the panel's render
// path to exercise unbound_capabilities + newly_stale_bindings.
function actionResult(name: string, overrides: Partial<InstalledActionResult> = {}): InstalledActionResult {
	return {
		name,
		fqn: `github://alice/conn-google/actions/${name}`,
		version: '1.2.0',
		source: `github://alice/conn-google/actions/${name}@1.2.0`,
		path: `~/.aileron/actions/${name}.md`,
		...overrides
	};
}

describe('HubCompositeInstallModal — install (action)', () => {
	it('disables Install when latest_version is absent', async () => {
		vi.mocked(getHubActionInstallDecision).mockResolvedValue({
			...actionDecision,
			latest_version: undefined
		});
		render(Modal, {
			props: { fqn: actionDecision.fqn, kind: 'action', onClose: () => {} }
		});
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-install-button')).toBeInTheDocument();
		});
		expect(screen.getByTestId('hub-composite-install-button')).toBeDisabled();
		expect(screen.getByTestId('hub-composite-no-version')).toBeInTheDocument();
		expect(installAction).not.toHaveBeenCalled();
	});

	it('enables Install once latest_version is set and dispatches with confirmed_fingerprints', async () => {
		vi.mocked(getHubActionInstallDecision).mockResolvedValue(actionDecision);
		vi.mocked(installAction).mockResolvedValue(actionResult('draft-email'));
		const user = userEvent.setup();
		render(Modal, {
			props: { fqn: actionDecision.fqn, kind: 'action', onClose: () => {} }
		});
		const btn = await screen.findByTestId('hub-composite-install-button');
		expect(btn).not.toBeDisabled();
		expect(screen.getByTestId('hub-composite-resolved-version')).toHaveTextContent('v1.2.0');

		await user.click(btn);

		await waitFor(() => {
			expect(installAction).toHaveBeenCalledTimes(1);
		});
		expect(installAction).toHaveBeenCalledWith({
			fqn: actionDecision.fqn,
			version: '1.2.0',
			auto_install_connectors: true,
			confirmed_fingerprints: [
				{
					fqn: 'github://alice/conn-google',
					fingerprint: 'sha256:aliceFingerprintAAAAAA'
				}
			]
		});
	});

	it('shows success panel with action name after install completes', async () => {
		vi.mocked(getHubActionInstallDecision).mockResolvedValue(actionDecision);
		vi.mocked(installAction).mockResolvedValue(actionResult('draft-email'));
		const user = userEvent.setup();
		render(Modal, {
			props: { fqn: actionDecision.fqn, kind: 'action', onClose: () => {} }
		});
		await user.click(await screen.findByTestId('hub-composite-install-button'));
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-success')).toBeInTheDocument();
		});
		expect(screen.getByTestId('hub-composite-installed-list')).toHaveTextContent('draft-email');
	});

	it('renders newly_stale_bindings panel when the install returns them', async () => {
		vi.mocked(getHubActionInstallDecision).mockResolvedValue(actionDecision);
		vi.mocked(installAction).mockResolvedValue(
			actionResult('draft-email', {
				newly_stale_bindings: [
					{
						name: 'oauth2/google/work',
						connector_fqn: 'github://alice/conn-google',
						stale_reason: 'scope_drift',
						missing_scopes: ['drive', 'documents'],
						service: 'google'
					}
				]
			})
		);
		const user = userEvent.setup();
		render(Modal, {
			props: { fqn: actionDecision.fqn, kind: 'action', onClose: () => {} }
		});
		await user.click(await screen.findByTestId('hub-composite-install-button'));
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-stale')).toBeInTheDocument();
		});
		const stale = screen.getByTestId('hub-composite-stale');
		expect(stale).toHaveTextContent('oauth2/google/work');
		expect(stale).toHaveTextContent('drive');
		expect(stale).toHaveTextContent('documents');
		expect(stale).toHaveTextContent('aileron binding reauthorize oauth2/google/work');
	});

	it('renders unbound_capabilities panel when present', async () => {
		vi.mocked(getHubActionInstallDecision).mockResolvedValue(actionDecision);
		vi.mocked(installAction).mockResolvedValue(
			actionResult('draft-email', {
				unbound_capabilities: [
					{ connector_fqn: 'github://alice/conn-google', kind: 'oauth2', scope: 'gmail.send' }
				]
			})
		);
		const user = userEvent.setup();
		render(Modal, {
			props: { fqn: actionDecision.fqn, kind: 'action', onClose: () => {} }
		});
		await user.click(await screen.findByTestId('hub-composite-install-button'));
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-unbound')).toBeInTheDocument();
		});
		expect(screen.getByTestId('hub-composite-unbound')).toHaveTextContent('github://alice/conn-google');
	});

	it('surfaces install errors and offers a retry', async () => {
		vi.mocked(getHubActionInstallDecision).mockResolvedValue(actionDecision);
		vi.mocked(installAction).mockRejectedValueOnce(new Error('hash_mismatch'));
		const user = userEvent.setup();
		render(Modal, {
			props: { fqn: actionDecision.fqn, kind: 'action', onClose: () => {} }
		});
		await user.click(await screen.findByTestId('hub-composite-install-button'));
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-install-error')).toHaveTextContent('hash_mismatch');
		});
		// Button flips to "Retry install".
		expect(screen.getByTestId('hub-composite-install-button')).toHaveTextContent('Retry install');
	});
});

describe('HubCompositeInstallModal — install (suite)', () => {
	it('walks every member_action with the suite-level fingerprints', async () => {
		vi.mocked(getHubSuiteInstallDecision).mockResolvedValue(suiteDecision);
		vi.mocked(installAction).mockImplementation(async ({ fqn }) =>
			actionResult(fqn.split('/').pop()!, { fqn, version: '2.1.0' })
		);
		const user = userEvent.setup();
		render(Modal, {
			props: { fqn: suiteDecision.fqn, kind: 'suite', onClose: () => {} }
		});
		await user.click(await screen.findByTestId('hub-composite-install-button'));
		await waitFor(() => {
			expect(installAction).toHaveBeenCalledTimes(suiteDecision.member_actions.length);
		});
		// Every call carries the suite's full fingerprint set.
		const expectedFingerprints = suiteDecision.authorities.map((a) => ({
			fqn: a.fqn,
			fingerprint: a.fingerprint
		}));
		for (let i = 0; i < suiteDecision.member_actions.length; i++) {
			expect(installAction).toHaveBeenNthCalledWith(i + 1, {
				fqn: suiteDecision.member_actions[i],
				version: '2.1.0',
				auto_install_connectors: true,
				confirmed_fingerprints: expectedFingerprints
			});
		}
	});

	it('aggregates and dedupes stale bindings across member installs', async () => {
		// Two member actions; the same connector upgrade fires the same
		// stale-binding transition on both. The panel must surface it
		// exactly once.
		vi.mocked(getHubSuiteInstallDecision).mockResolvedValue(suiteDecision);
		const stale = {
			name: 'oauth2/google/work',
			connector_fqn: 'github://alice/conn-google',
			stale_reason: 'no_grant_record' as const,
			service: 'google'
		};
		vi.mocked(installAction).mockImplementation(async ({ fqn }) =>
			actionResult(fqn.split('/').pop()!, { fqn, newly_stale_bindings: [stale] })
		);
		const user = userEvent.setup();
		render(Modal, {
			props: { fqn: suiteDecision.fqn, kind: 'suite', onClose: () => {} }
		});
		await user.click(await screen.findByTestId('hub-composite-install-button'));
		await waitFor(() => {
			expect(screen.getByTestId('hub-composite-stale')).toBeInTheDocument();
		});
		const stalePanel = screen.getByTestId('hub-composite-stale');
		// "oauth2/google/work" should appear exactly once in the list.
		const occurrences = stalePanel.textContent?.match(/oauth2\/google\/work/g) ?? [];
		// Two occurrences: one in the binding name, one in the CLI command hint.
		expect(occurrences.length).toBe(2);
	});

	it('suite Install button label mentions the member count', async () => {
		vi.mocked(getHubSuiteInstallDecision).mockResolvedValue(suiteDecision);
		render(Modal, {
			props: { fqn: suiteDecision.fqn, kind: 'suite', onClose: () => {} }
		});
		const btn = await screen.findByTestId('hub-composite-install-button');
		expect(btn).toHaveTextContent(`Install suite (${suiteDecision.member_actions.length})`);
	});
});
