import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';

vi.mock('$lib/api', () => ({
	listInstalledActions: vi.fn(),
	setActionEnabled: vi.fn()
}));

import Page from './+page.svelte';
import {
	listInstalledActions,
	setActionEnabled,
	type InstalledAction
} from '$lib/api';

const exampleEnabled: InstalledAction = {
	name: 'ship-update',
	version: '1.0.0',
	source: 'hub://aileron/ship-update@1.0.0',
	enabled: true
};

const exampleDisabled: InstalledAction = {
	name: 'list-emails',
	version: '2.1.0',
	source: 'hub://aileron/list-emails@2.1.0',
	enabled: false
};

beforeEach(() => {
	vi.mocked(listInstalledActions).mockReset();
	vi.mocked(setActionEnabled).mockReset();
});

describe('Actions page — listing', () => {
	it('renders each installed action with its enabled state', async () => {
		vi.mocked(listInstalledActions).mockResolvedValue({
			items: [exampleEnabled, exampleDisabled]
		});

		render(Page);

		await waitFor(() => {
			expect(screen.getByText('ship-update')).toBeInTheDocument();
			expect(screen.getByText('list-emails')).toBeInTheDocument();
		});
		const toggles = screen.getAllByTestId('action-toggle') as HTMLInputElement[];
		expect(toggles).toHaveLength(2);
		expect(toggles[0].checked).toBe(true);
		expect(toggles[1].checked).toBe(false);
	});

	it('shows the empty-state hint when nothing is installed', async () => {
		vi.mocked(listInstalledActions).mockResolvedValue({ items: [] });
		render(Page);
		await waitFor(() => {
			expect(screen.getByText(/No actions installed/)).toBeInTheDocument();
		});
	});

	it('surfaces load errors below the inventory', async () => {
		vi.mocked(listInstalledActions).mockResolvedValue({
			items: [exampleEnabled],
			load_errors: [
				{ file: '/path/to/broken.md', message: 'parse error', class: 'parse_error' }
			]
		});
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('load-errors-section')).toBeInTheDocument();
			expect(screen.getByText(/broken\.md/)).toBeInTheDocument();
		});
	});

	it('shows the error state when the daemon call fails', async () => {
		vi.mocked(listInstalledActions).mockRejectedValue(new Error('boom'));
		render(Page);
		await waitFor(() => {
			expect(screen.getByTestId('actions-error')).toHaveTextContent('boom');
		});
	});
});

describe('Actions page — toggle', () => {
	it('PATCHes the daemon and reflects the new state in the row', async () => {
		vi.mocked(listInstalledActions).mockResolvedValue({
			items: [exampleEnabled]
		});
		vi.mocked(setActionEnabled).mockResolvedValue({
			...exampleEnabled,
			enabled: false
		});

		render(Page);

		const toggle = (await screen.findByTestId('action-toggle')) as HTMLInputElement;
		expect(toggle.checked).toBe(true);

		await fireEvent.click(toggle);

		await waitFor(() => {
			expect(vi.mocked(setActionEnabled)).toHaveBeenCalledWith('ship-update', false);
		});
		await waitFor(() => {
			const updated = screen.getByTestId('action-toggle') as HTMLInputElement;
			expect(updated.checked).toBe(false);
		});
	});

	it('re-enables a disabled action with one click', async () => {
		vi.mocked(listInstalledActions).mockResolvedValue({
			items: [exampleDisabled]
		});
		vi.mocked(setActionEnabled).mockResolvedValue({
			...exampleDisabled,
			enabled: true
		});

		render(Page);
		const toggle = (await screen.findByTestId('action-toggle')) as HTMLInputElement;
		expect(toggle.checked).toBe(false);

		await fireEvent.click(toggle);

		await waitFor(() => {
			expect(vi.mocked(setActionEnabled)).toHaveBeenCalledWith('list-emails', true);
		});
	});
});
