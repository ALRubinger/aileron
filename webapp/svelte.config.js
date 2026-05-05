import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/**
 * Local-webapp config. Produces a fully static build (no Node server)
 * so the Go daemon can `//go:embed` the output and serve it at the
 * gateway origin under `aileron launch`. No `+page.server.ts` or
 * `+server.ts` allowed — every request goes through the Aileron API
 * the daemon already exposes.
 *
 * `fallback: 'index.html'` enables SPA-style client-side routing:
 * any unknown path resolves to index.html and the Svelte router
 * renders the page. Without this, /approvals would 404 on direct
 * navigation (only / would be prerendered).
 *
 * @type {import('@sveltejs/kit').Config}
 */
const config = {
	compilerOptions: {
		runes: true
	},
	preprocess: vitePreprocess(),
	kit: {
		adapter: adapter({
			fallback: 'index.html'
		})
	}
};

export default config;
