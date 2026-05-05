<script lang="ts">
	import '../app.css';

	// Local-webapp shell. Single tab focus: the daemon serves this on
	// the same origin the agent talks to, so there's no auth, no
	// multi-user header, no organization selector — just the bare
	// chrome that frames the approvals queue.
	let { children } = $props();
</script>

<svelte:head>
	<title>Aileron</title>
</svelte:head>

<header class="border-b border-border bg-card">
	<div class="mx-auto flex max-w-4xl items-center justify-between px-4 py-3">
		<a href="/" class="text-sm font-semibold tracking-tight no-underline">
			✈️ Aileron
		</a>
		<nav class="flex items-center gap-4 text-sm">
			<a href="/approvals" class="text-foreground hover:text-primary no-underline"
				>Approvals</a
			>
		</nav>
	</div>
</header>

<main class="mx-auto max-w-4xl px-4 py-6">
	{@render children?.()}
</main>
