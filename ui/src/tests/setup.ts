import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/svelte';
import { afterEach, vi } from 'vitest';

afterEach(() => {
	cleanup();
});

// Make window.location writable for tests that set location.href
Object.defineProperty(window, 'location', {
	writable: true,
	value: { ...window.location, href: '', origin: 'http://localhost:3000' }
});

// Polyfill ResizeObserver for bits-ui components in jsdom
global.ResizeObserver = vi.fn().mockImplementation(() => ({
	observe: vi.fn(),
	unobserve: vi.fn(),
	disconnect: vi.fn()
}));
