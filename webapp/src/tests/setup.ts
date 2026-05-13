import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/svelte';
import { afterEach, vi } from 'vitest';

afterEach(async () => {
	cleanup();
	// bits-ui's body-scroll-lock schedules a delayed `document.body`
	// reset on unmount (24ms default) for its internal Tooltip /
	// FloatingLayer machinery. If the timer fires after jsdom has been
	// torn down — which happens on slower CI hosts between test
	// boundaries — it crashes with "document is not defined" as an
	// unhandled error and fails the run even when every test passed.
	// Wait long enough for the cleanup callback to run in the still-
	// alive jsdom env.
	await new Promise((resolve) => setTimeout(resolve, 50));
});

// Polyfill ResizeObserver for bits-ui components in jsdom. Without this,
// any component that uses bits-ui internals — Card, Button, Dialog —
// throws on mount because the underlying floating-ui-svelte machinery
// expects ResizeObserver to be available globally.
//
// `globalThis` rather than `global` so this also type-checks under
// the bundler module-resolution we use for the webapp (no @types/node
// dep — keeps the install lean since the daemon hosts the runtime).
globalThis.ResizeObserver = vi.fn().mockImplementation(() => ({
	observe: vi.fn(),
	unobserve: vi.fn(),
	disconnect: vi.fn()
}));
