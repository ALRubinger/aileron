<script lang="ts">
	import * as Card from '$lib/components/ui/card';
</script>

<svelte:head>
	<title>Aileron</title>
</svelte:head>

<h1 class="text-2xl font-semibold mb-2">Aileron</h1>
<p class="text-muted-foreground">Agentic control plane</p>

<div class="grid grid-cols-[repeat(auto-fit,minmax(200px,1fr))] gap-4 mt-8">
	{#each [
		{ href: '/approvals', title: 'Approvals', desc: 'Review and act on pending approval requests' },
		{ href: '/traces', title: 'Traces', desc: 'Audit trail of all control plane events' },
		{ href: '/policies', title: 'Policies', desc: 'View and manage policy rules' },
		{ href: '/marketplace', title: 'Marketplace', desc: 'Browse and install MCP servers from the registry' },
		{ href: '/servers', title: 'Servers', desc: 'Manage installed MCP server configurations' },
	] as item}
		<a href={item.href} class="no-underline text-foreground">
			<Card.Root class="h-full hover:bg-muted/50 transition-colors">
				<Card.Header>
					<Card.Title>{item.title}</Card.Title>
					<Card.Description>{item.desc}</Card.Description>
				</Card.Header>
			</Card.Root>
		</a>
	{/each}
</div>
