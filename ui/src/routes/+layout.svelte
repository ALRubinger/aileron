<script>
	let { children } = $props();
</script>

<svelte:head>
	<style>
		:root {
			--bg: #0a0a0a;
			--bg-card: #141414;
			--bg-hover: #1a1a1a;
			--border: #2a2a2a;
			--text: #e5e5e5;
			--text-muted: #888;
			--accent: #3b82f6;
			--green: #22c55e;
			--red: #ef4444;
			--yellow: #eab308;
			--orange: #f97316;
			font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif;
		}
		body {
			margin: 0;
			background: var(--bg);
			color: var(--text);
		}
	</style>
</svelte:head>

<nav style="border-bottom: 1px solid var(--border); padding: 0.75rem 1.5rem; display: flex; align-items: center; gap: 2rem;">
	<a href="/" style="font-weight: 700; font-size: 1.1rem; text-decoration: none; color: var(--text);">Aileron</a>
	<a href="/approvals" style="text-decoration: none; color: var(--text-muted); font-size: 0.9rem;">Approvals</a>
	<a href="/traces" style="text-decoration: none; color: var(--text-muted); font-size: 0.9rem;">Traces</a>
	<a href="/policies" style="text-decoration: none; color: var(--text-muted); font-size: 0.9rem;">Policies</a>
</nav>

<main style="max-width: 960px; margin: 0 auto; padding: 1.5rem;">
	{@render children()}
</main>
