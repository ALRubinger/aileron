<script>
	import '../app.css';
	let { children } = $props();
</script>

<svelte:head>
	<title>Aileron</title>
</svelte:head>

<nav class="border-b border-border px-6 py-3 flex items-center gap-8">
	<a href="/" class="font-bold text-lg no-underline text-foreground">Aileron</a>
	<a href="/approvals" class="no-underline text-muted-foreground text-sm">Approvals</a>
	<a href="/traces" class="no-underline text-muted-foreground text-sm">Traces</a>
	<a href="/policies" class="no-underline text-muted-foreground text-sm">Policies</a>
	<a href="/marketplace" class="no-underline text-muted-foreground text-sm">Marketplace</a>
	<a href="/servers" class="no-underline text-muted-foreground text-sm">Servers</a>
</nav>

<main class="max-w-[960px] mx-auto p-6">
	{@render children()}
</main>
