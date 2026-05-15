import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	getHubActionInstallDecision: vi.fn(),
	getHubSuiteInstallDecision: vi.fn()
}));

import Modal from './HubCompositeInstallModal.svelte';
import {
	getHubActionInstallDecision,
	getHubSuiteInstallDecision,
	type HubActionInstallDecision,
	type HubSuiteInstallDecision
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
	]
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
	]
};

beforeEach(() => {
	vi.mocked(getHubActionInstallDecision).mockReset();
	vi.mocked(getHubSuiteInstallDecision).mockReset();
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
