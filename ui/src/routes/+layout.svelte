<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { Button } from '$lib/components/ui/button';
	import { initAuth, isAuthenticated, getUser, logout } from '$lib/auth.svelte.js';

	let { children } = $props();
	let mounted = $state(false);

	const publicPaths = ['/login', '/signup', '/verify-email', '/auth/callback'];

	onMount(() => {
		initAuth();
		mounted = true;
	});

	$effect(() => {
		if (!mounted) return;
		const path = page.url.pathname;
		const isPublic = publicPaths.some((p) => path.startsWith(p));
		if (!isPublic && !isAuthenticated()) {
			goto('/login');
		}
	});

	async function handleLogout() {
		await logout();
		goto('/login');
	}
</script>

<svelte:head>
	<title>Aileron</title>
</svelte:head>

<nav class="border-b border-border px-6 py-3 flex items-center gap-8">
	<a href="/" class="font-bold text-lg no-underline text-foreground">Aileron</a>

	{#if isAuthenticated()}
		<a href="/approvals" class="no-underline text-muted-foreground text-sm">Approvals</a>
		<a href="/traces" class="no-underline text-muted-foreground text-sm">Traces</a>
		<a href="/policies" class="no-underline text-muted-foreground text-sm">Policies</a>
		<a href="/marketplace" class="no-underline text-muted-foreground text-sm">Marketplace</a>
		<a href="/servers" class="no-underline text-muted-foreground text-sm">Servers</a>

		<div class="ml-auto flex items-center gap-3">
			{#if getUser()}
				<span class="text-sm text-muted-foreground">{getUser()?.email}</span>
			{/if}
			<Button variant="ghost" size="sm" onclick={handleLogout}>
				Log out
			</Button>
		</div>
	{/if}
</nav>

<main class="max-w-[960px] mx-auto p-6">
	{@render children()}
</main>
