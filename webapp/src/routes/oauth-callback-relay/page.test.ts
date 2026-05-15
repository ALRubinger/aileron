import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render } from '@testing-library/svelte';

import Relay from './+page.svelte';

// The relay page (#743 PR 3) is the popup landing page after the
// daemon-hosted OAuth callback writes the binding. It reads the
// fragment, postMessages the opener, and closes itself. These tests
// stub window.opener / window.close and observe the postMessage
// call shape.

beforeEach(() => {
	// Default to an empty hash; individual tests override.
	window.location.hash = '';
});

afterEach(() => {
	window.location.hash = '';
});

describe('oauth-callback-relay', () => {
	it('postMessages the opener with parsed fragment + closes', async () => {
		window.location.hash = '#aileron_oauth=ok&binding=oauth2%2Fgoogle%2Fwork';
		const postMessage = vi.fn();
		const opener = { postMessage } as unknown as Window;
		Object.defineProperty(window, 'opener', { value: opener, configurable: true });
		const close = vi.spyOn(window, 'close').mockImplementation(() => {});

		render(Relay);
		// onMount + the deferred close timer (50ms) need to flush.
		await new Promise((resolve) => setTimeout(resolve, 100));

		expect(postMessage).toHaveBeenCalledWith(
			{
				type: 'aileron-oauth-callback',
				status: 'ok',
				binding: 'oauth2/google/work'
			},
			window.location.origin
		);
		expect(close).toHaveBeenCalled();
	});

	it('still works when opener is null — renders a manual-close hint', async () => {
		window.location.hash = '#aileron_oauth=ok&binding=oauth2%2Fgoogle%2Fwork';
		Object.defineProperty(window, 'opener', { value: null, configurable: true });
		const close = vi.spyOn(window, 'close').mockImplementation(() => {});

		const { getByText } = render(Relay);
		await new Promise((resolve) => setTimeout(resolve, 50));

		expect(getByText(/Reauthorized/)).toBeInTheDocument();
		expect(close).not.toHaveBeenCalled();
	});

	it('surfaces error status when opener is absent', async () => {
		window.location.hash = '#aileron_oauth=error';
		Object.defineProperty(window, 'opener', { value: null, configurable: true });

		const { getByText } = render(Relay);
		await new Promise((resolve) => setTimeout(resolve, 50));

		expect(getByText(/Reauthorize was not completed/)).toBeInTheDocument();
	});
});
