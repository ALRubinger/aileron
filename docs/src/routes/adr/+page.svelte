<script lang="ts">
	let { data } = $props();
</script>

<svelte:head>
	<title>Architecture Decision Records - Aileron Docs</title>
</svelte:head>

<h1>Architecture Decision Records</h1>

<p class="text-muted-foreground">
	Design decisions and their rationale, documented as they are made.
</p>

<ul>
	{#each data.adrs as adr}
		<li>
			<a href="/adr/{adr.slug}">{adr.title}</a>
		</li>
	{/each}
</ul>
