import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';

// Mock $lib/api before importing the component
vi.mock('$lib/api', () => ({
	listConnectedAccounts: vi.fn(),
	deleteConnectedAccount: vi.fn()
}));

// Mock $lib/colors — use real implementation
vi.mock('$lib/colors', async () => {
	return await vi.importActual('$lib/colors');
});

import Page from './+page.svelte';
import { listConnectedAccounts, deleteConnectedAccount } from '$lib/api';

const mockAccounts = [
	{
		id: 'conn_1',
		provider: 'slack',
		email: 'user@workspace.slack.com',
		status: 'active',
		scopes: ['channels:read', 'chat:write'],
		created_at: '2025-01-15T00:00:00Z',
		updated_at: '2025-01-15T00:00:00Z'
	},
	{
		id: 'conn_2',
		provider: 'github_repos',
		email: 'user@github.com',
		status: 'expired',
		scopes: ['repo'],
		created_at: '2025-02-20T00:00:00Z',
		updated_at: '2025-03-01T00:00:00Z'
	}
];

beforeEach(() => {
	vi.mocked(listConnectedAccounts).mockResolvedValue({ items: mockAccounts });
	vi.mocked(deleteConnectedAccount).mockResolvedValue(null);
});

describe('Connected Accounts Page', () => {
	it('shows loading state initially', () => {
		vi.mocked(listConnectedAccounts).mockReturnValue(new Promise(() => {})); // never resolves
		render(Page);
		expect(screen.getByText('Loading...')).toBeInTheDocument();
	});

	it('renders connected accounts after loading', async () => {
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('Slack')).toBeInTheDocument();
		});
		expect(screen.getByText('GitHub')).toBeInTheDocument();
		expect(screen.getByText('user@workspace.slack.com')).toBeInTheDocument();
		expect(screen.getByText('user@github.com')).toBeInTheDocument();
	});

	it('shows status badges for each account', async () => {
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('active')).toBeInTheDocument();
		});
		expect(screen.getByText('expired')).toBeInTheDocument();
	});

	it('shows available integrations for unconnected providers', async () => {
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('Gmail')).toBeInTheDocument();
		});
		expect(screen.getByText('Google Calendar')).toBeInTheDocument();
	});

	it('shows empty state when no accounts connected', async () => {
		vi.mocked(listConnectedAccounts).mockResolvedValue({ items: [] });
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('No accounts connected yet.')).toBeInTheDocument();
		});
	});

	it('shows all providers as available when no accounts exist', async () => {
		vi.mocked(listConnectedAccounts).mockResolvedValue({ items: [] });
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('Slack')).toBeInTheDocument();
		});
		expect(screen.getByText('GitHub')).toBeInTheDocument();
		expect(screen.getByText('Gmail')).toBeInTheDocument();
		expect(screen.getByText('Google Calendar')).toBeInTheDocument();
	});

	it('shows error message when API fails', async () => {
		vi.mocked(listConnectedAccounts).mockRejectedValue(new Error('Network error'));
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('Network error')).toBeInTheDocument();
		});
	});

	it('sets location.href when Connect is clicked', async () => {
		vi.mocked(listConnectedAccounts).mockResolvedValue({ items: [] });
		const user = userEvent.setup();
		render(Page);

		await waitFor(() => {
			expect(screen.getByText('Slack')).toBeInTheDocument();
		});

		// Find Connect buttons — there should be one per available provider
		const connectButtons = screen.getAllByRole('button', { name: 'Connect' });
		await user.click(connectButtons[0]); // First provider alphabetically by key: github_repos

		expect(window.location.href).toContain('/v1/connect/');
	});

	it('shows disconnect confirmation dialog', async () => {
		const user = userEvent.setup();
		render(Page);

		await waitFor(() => {
			expect(screen.getByText('Slack')).toBeInTheDocument();
		});

		const disconnectButtons = screen.getAllByRole('button', { name: 'Disconnect' });
		await user.click(disconnectButtons[0]);

		await waitFor(() => {
			expect(screen.getByText(/Disconnect Slack\?/)).toBeInTheDocument();
		});
	});

	it('calls deleteConnectedAccount on disconnect confirm', async () => {
		const user = userEvent.setup({ pointerEventsCheck: 0 });
		render(Page);

		await waitFor(() => {
			expect(screen.getByText('Slack')).toBeInTheDocument();
		});

		// Click Disconnect on first account
		const disconnectButtons = screen.getAllByRole('button', { name: 'Disconnect' });
		await user.click(disconnectButtons[0]);

		// Wait for dialog
		await waitFor(() => {
			expect(screen.getByText(/Disconnect Slack\?/)).toBeInTheDocument();
		});

		// After disconnect, listConnectedAccounts is called again to refresh
		vi.mocked(listConnectedAccounts).mockResolvedValue({ items: [mockAccounts[1]] });

		// Find the confirm Disconnect button inside the dialog (destructive variant)
		// Use pointerEventsCheck: 0 because bits-ui dialog overlay affects pointer-events
		const allDisconnectBtns = screen.getAllByRole('button', { name: 'Disconnect' });
		// The dialog button is the last one (destructive variant)
		const dialogDisconnect = allDisconnectBtns[allDisconnectBtns.length - 1];
		await user.click(dialogDisconnect);

		await waitFor(() => {
			expect(deleteConnectedAccount).toHaveBeenCalledWith('conn_1');
		});
	});

	it('handles array response without items wrapper', async () => {
		vi.mocked(listConnectedAccounts).mockResolvedValue(mockAccounts);
		render(Page);
		await waitFor(() => {
			expect(screen.getByText('Slack')).toBeInTheDocument();
		});
	});
});
