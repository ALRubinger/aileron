import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	getHubInstallDecision: vi.fn(),
	installConnector: vi.fn()
}));

import HubInstallModal from './HubInstallModal.svelte';
import {
	getHubInstallDecision,
	installConnector,
	type HubInstallDecision
} from '$lib/api';

const unknownDecision: HubInstallDecision = {
	fqn: 'github://alice/calendar',
	description: 'Google Calendar connector',
	publisher_github: 'alice',
	fingerprint: 'sha256:aliceFingerprintAAAAAAA',
	trust_state: 'unknown',
	publisher_footprint: ['github://alice/drive', 'github://alice/gmail'],
	risk_indicators: ["First connector by this publisher you've installed"]
};

const conflictDecision: HubInstallDecision = {
	fqn: 'github://acme/x',
	description: 'malicious twin',
	publisher_github: 'acme',
	fingerprint: 'sha256:newFingerprintBBBBBBBB',
	trust_state: 'conflict',
	publisher_footprint: ['github://acme/y'],
	risk_indicators: [
		'Key fingerprint differs from one you trust for github://acme/y'
	]
};

const alreadyTrustedDecision: HubInstallDecision = {
	fqn: 'github://acme/z',
	description: 'previously trusted',
	publisher_github: 'acme',
	fingerprint: 'sha256:knownFingerprintCCCCCCC',
	trust_state: 'already_trusted',
	publisher_footprint: [],
	risk_indicators: []
};

beforeEach(() => {
	vi.mocked(getHubInstallDecision).mockReset();
	vi.mocked(installConnector).mockReset();
});

describe('HubInstallModal — fetch + render', () => {
	it('does not call the API while fqn is null', () => {
		render(HubInstallModal, { fqn: null, onClose: () => {} });
		expect(getHubInstallDecision).not.toHaveBeenCalled();
	});

	it('renders FQN, fingerprint, publisher, and risk indicators', async () => {
		vi.mocked(getHubInstallDecision).mockResolvedValue(unknownDecision);
		render(HubInstallModal, {
			fqn: 'github://alice/calendar',
			onClose: () => {}
		});
		await waitFor(() => {
			expect(getHubInstallDecision).toHaveBeenCalledWith(
				'github://alice/calendar'
			);
		});
		await waitFor(() => {
			expect(screen.getByText(unknownDecision.fingerprint)).toBeInTheDocument();
			expect(screen.getByText('alice')).toBeInTheDocument();
			expect(
				screen.getByText("First connector by this publisher you've installed")
			).toBeInTheDocument();
			expect(screen.getByText('github://alice/drive')).toBeInTheDocument();
		});
	});

	it('surfaces a decision fetch error rather than silently rendering empty', async () => {
		vi.mocked(getHubInstallDecision).mockRejectedValue(
			new Error('hub_unreachable')
		);
		render(HubInstallModal, {
			fqn: 'github://alice/calendar',
			onClose: () => {}
		});
		await waitFor(() => {
			expect(screen.getByTestId('hub-install-decision-error')).toHaveTextContent(
				'hub_unreachable'
			);
		});
	});

	it('marks conflict trust state with the destructive Badge variant', async () => {
		vi.mocked(getHubInstallDecision).mockResolvedValue(conflictDecision);
		render(HubInstallModal, { fqn: conflictDecision.fqn, onClose: () => {} });
		await waitFor(() => {
			const badge = screen.getByTestId('hub-trust-state');
			expect(badge).toHaveAttribute('data-trust-state', 'conflict');
		});
	});

	it('marks already_trusted with the affirmative Badge variant', async () => {
		vi.mocked(getHubInstallDecision).mockResolvedValue(alreadyTrustedDecision);
		render(HubInstallModal, {
			fqn: alreadyTrustedDecision.fqn,
			onClose: () => {}
		});
		await waitFor(() => {
			const badge = screen.getByTestId('hub-trust-state');
			expect(badge).toHaveAttribute('data-trust-state', 'already_trusted');
		});
	});
});

describe('HubInstallModal — install action', () => {
	it('disables Install until a version is provided', async () => {
		vi.mocked(getHubInstallDecision).mockResolvedValue(unknownDecision);
		render(HubInstallModal, { fqn: unknownDecision.fqn, onClose: () => {} });
		const button = (await screen.findByTestId(
			'hub-install-button'
		)) as HTMLButtonElement;
		expect(button.disabled).toBe(true);
		const versionInput = (await screen.findByTestId(
			'hub-install-version-input'
		)) as HTMLInputElement;
		await fireEvent.input(versionInput, { target: { value: '1.2.0' } });
		await waitFor(() => {
			expect(button.disabled).toBe(false);
		});
	});

	it('sends confirmed_fingerprint and renders the installed envelope', async () => {
		vi.mocked(getHubInstallDecision).mockResolvedValue(unknownDecision);
		vi.mocked(installConnector).mockResolvedValue({
			fqn: unknownDecision.fqn,
			version: '1.2.0',
			hash: 'sha256:abc123',
			entry_dir: '/.aileron/store/connectors/sha256/abc123'
		});
		render(HubInstallModal, { fqn: unknownDecision.fqn, onClose: () => {} });
		const versionInput = (await screen.findByTestId(
			'hub-install-version-input'
		)) as HTMLInputElement;
		await fireEvent.input(versionInput, { target: { value: '1.2.0' } });
		await fireEvent.click(await screen.findByTestId('hub-install-button'));
		await waitFor(() => {
			expect(installConnector).toHaveBeenCalledWith({
				fqn: unknownDecision.fqn,
				version: '1.2.0',
				confirmed_fingerprint: unknownDecision.fingerprint,
				expected_hash: undefined
			});
		});
		await waitFor(() => {
			expect(screen.getByTestId('hub-install-installed')).toHaveTextContent(
				'Installed'
			);
			expect(screen.getByText('sha256:abc123')).toBeInTheDocument();
		});
	});

	it('forwards expected_hash when the operator supplies one', async () => {
		vi.mocked(getHubInstallDecision).mockResolvedValue(unknownDecision);
		vi.mocked(installConnector).mockResolvedValue({
			fqn: unknownDecision.fqn,
			version: '1.2.0',
			hash: 'sha256:abc',
			entry_dir: '/p'
		});
		render(HubInstallModal, { fqn: unknownDecision.fqn, onClose: () => {} });
		await fireEvent.input(await screen.findByTestId('hub-install-version-input'), {
			target: { value: '1.2.0' }
		});
		await fireEvent.input(await screen.findByTestId('hub-install-hash-input'), {
			target: { value: 'sha256:expected' }
		});
		await fireEvent.click(await screen.findByTestId('hub-install-button'));
		await waitFor(() => {
			expect(installConnector).toHaveBeenCalledWith(
				expect.objectContaining({ expected_hash: 'sha256:expected' })
			);
		});
	});

	it('renders "Already installed" verb when daemon short-circuits', async () => {
		vi.mocked(getHubInstallDecision).mockResolvedValue(unknownDecision);
		vi.mocked(installConnector).mockResolvedValue({
			fqn: unknownDecision.fqn,
			version: '1.2.0',
			hash: 'sha256:abc',
			entry_dir: '/p',
			already_installed: true
		});
		render(HubInstallModal, { fqn: unknownDecision.fqn, onClose: () => {} });
		await fireEvent.input(await screen.findByTestId('hub-install-version-input'), {
			target: { value: '1.2.0' }
		});
		await fireEvent.click(await screen.findByTestId('hub-install-button'));
		await waitFor(() => {
			expect(screen.getByTestId('hub-install-installed')).toHaveTextContent(
				'Already installed'
			);
		});
	});

	it('surfaces install errors and does not render the installed envelope', async () => {
		vi.mocked(getHubInstallDecision).mockResolvedValue(unknownDecision);
		vi.mocked(installConnector).mockRejectedValue(
			new Error('publisher key fingerprint changed')
		);
		render(HubInstallModal, { fqn: unknownDecision.fqn, onClose: () => {} });
		await fireEvent.input(await screen.findByTestId('hub-install-version-input'), {
			target: { value: '1.2.0' }
		});
		await fireEvent.click(await screen.findByTestId('hub-install-button'));
		await waitFor(() => {
			expect(screen.getByTestId('hub-install-error')).toHaveTextContent(
				'fingerprint changed'
			);
		});
		expect(screen.queryByTestId('hub-install-installed')).toBeNull();
	});
});
