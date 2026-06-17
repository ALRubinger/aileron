import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';

import Page from './+page.svelte';

describe('Security settings page', () => {
	it('describes the local vault encryption scheme', () => {
		render(Page);

		expect(screen.getByText('Vault encryption')).toBeInTheDocument();
		expect(screen.getByText('Argon2id (passphrase-derived key)')).toBeInTheDocument();
		expect(screen.getByText('AES-256-GCM envelope encryption')).toBeInTheDocument();
		expect(screen.getByText('Derived in your browser')).toBeInTheDocument();
	});
});
