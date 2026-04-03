<script lang="ts">
	import { page } from '$app/state';
	import { Separator } from '$lib/components/ui/separator';

	let { children } = $props();

	const navItems = [
		{ href: '/settings/profile', label: 'Profile' },
		{ href: '/settings/organization', label: 'Organization' }
	];
</script>

<svelte:head>
	<title>Settings - Aileron</title>
</svelte:head>

<h1 class="text-xl font-semibold mb-1">Settings</h1>
<p class="text-sm text-muted-foreground mb-6">Manage your account and organization</p>

<Separator />

<div class="flex gap-8 mt-6">
	<nav class="flex flex-col gap-1 w-48 shrink-0">
		{#each navItems as item}
			<a
				href={item.href}
				class="text-sm px-3 py-1.5 rounded-md no-underline transition-colors {page.url.pathname ===
				item.href
					? 'bg-muted font-medium text-foreground'
					: 'text-muted-foreground hover:text-foreground'}"
			>
				{item.label}
			</a>
		{/each}
	</nav>

	<div class="flex-1 min-w-0">
		{@render children()}
	</div>
</div>
