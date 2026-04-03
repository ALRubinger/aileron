<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { Button } from '$lib/components/ui/button';
	import { Separator } from '$lib/components/ui/separator';
	import { initAuth, isAuthenticated, getUser, logout } from '$lib/auth.svelte.js';

	let { children } = $props();
	let mounted = $state(false);
	let menuOpen = $state(false);

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

	// Close menu when clicking outside.
	function handleWindowClick() {
		if (menuOpen) menuOpen = false;
	}

	async function handleLogout() {
		menuOpen = false;
		await logout();
		goto('/login');
	}
</script>

<svelte:window onclick={handleWindowClick} />

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

		<div class="ml-auto relative">
			<button
				class="flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground transition-colors cursor-pointer bg-transparent border-0 p-0"
				onclick={(e) => {
					e.stopPropagation();
					menuOpen = !menuOpen;
				}}
			>
				{#if getUser()?.avatar_url}
					<img
						src={getUser()?.avatar_url}
						alt=""
						class="w-6 h-6 rounded-full"
						referrerpolicy="no-referrer"
					/>
				{/if}
				<span>{getUser()?.display_name || getUser()?.email}</span>
				<svg
					class="w-3 h-3 opacity-60"
					fill="none"
					viewBox="0 0 10 6"
					stroke="currentColor"
					stroke-width="2"
				>
					<path d="M1 1l4 4 4-4" />
				</svg>
			</button>

			{#if menuOpen}
				<div
					class="absolute right-0 mt-2 w-48 bg-popover border border-border rounded-md shadow-md z-50 py-1"
					role="menu"
					tabindex="-1"
					onclick={(e) => e.stopPropagation()}
					onkeydown={(e) => {
						if (e.key === 'Escape') menuOpen = false;
					}}
				>
					<div class="px-3 py-2 text-xs text-muted-foreground truncate">
						{getUser()?.email}
					</div>
					<Separator />
					<a
						href="/settings/profile"
						class="block px-3 py-2 text-sm no-underline text-foreground hover:bg-muted transition-colors"
						onclick={() => (menuOpen = false)}
					>
						Settings
					</a>
					<Separator />
					<button
						class="w-full text-left px-3 py-2 text-sm text-foreground hover:bg-muted transition-colors cursor-pointer bg-transparent border-0"
						onclick={handleLogout}
					>
						Log out
					</button>
				</div>
			{/if}
		</div>
	{/if}
</nav>

<main class="max-w-[960px] mx-auto p-6">
	{@render children()}
</main>
