import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	getTeeStatus: vi.fn()
}));

import Page from './+page.svelte';
import { getTeeStatus } from '$lib/api';

beforeEach(() => {
	vi.mocked(getTeeStatus).mockResolvedValue({
		enabled: true,
		provider: 'confidential-space',
		attested: true,
		expected_identity: {
			image_digest: 'sha256:expected',
			project_id: 'expected-project'
		},
		attestation_claims: {
			image_digest: 'sha256:expected',
			project_id: 'expected-project',
			hwmodel: 'GCP_AMD_SEV',
			issuer: 'https://accounts.google.com',
			issued_at: '2026-04-29T10:00:00Z',
			expires_at: '2026-04-29T11:00:00Z'
		}
	});
});
describe('Security settings page', () => {
	it('shows expected and attested enclave identity', async () => {
		render(Page);

		await waitFor(() => {
			expect(screen.getByText('Verified by Google Confidential Computing')).toBeInTheDocument();
		});
		expect(screen.getByText('Expected enclave identity')).toBeInTheDocument();
		expect(screen.getAllByText('sha256:expected')).toHaveLength(2);
		expect(screen.getAllByText('expected-project')).toHaveLength(2);
		expect(screen.getByText('GCP_AMD_SEV')).toBeInTheDocument();
	});

	it('shows API errors', async () => {
		vi.mocked(getTeeStatus).mockRejectedValue(new Error('status unavailable'));

		render(Page);

		await waitFor(() => {
			expect(screen.getByText('status unavailable')).toBeInTheDocument();
		});
	});
});
