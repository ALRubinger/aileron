<script lang="ts">
	import '../app.css';

	let { children } = $props();
</script>

<nav class="border-b border-border px-6 py-3 flex items-center gap-6">
	<a href="/" class="font-bold text-lg no-underline text-foreground hover:text-foreground">
		Aileron Docs
	</a>
	<a href="/api" class="text-sm text-muted-foreground hover:text-foreground transition-colors">
		API Reference
	</a>
	<a href="/adr" class="text-sm text-muted-foreground hover:text-foreground transition-colors">
		Architecture Decisions
	</a>
</nav>

<main class="max-w-4xl mx-auto px-6 py-8 prose">
	{@render children()}
</main>
