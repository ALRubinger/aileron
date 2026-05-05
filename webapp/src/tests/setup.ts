import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/svelte';
import { afterEach, vi } from 'vitest';

afterEach(() => {
	cleanup();
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
