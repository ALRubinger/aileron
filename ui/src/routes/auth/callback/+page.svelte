<script lang="ts">
	import { goto } from '$app/navigation';
	import { PUBLIC_API_BASE } from '$env/static/public';
	import { setAuth } from '$lib/auth.js';
	import { onMount } from 'svelte';

	let error = $state('');

	onMount(async () => {
		try {
			// After OAuth redirect, the server set httpOnly cookies.
			// Fetch user profile using cookie auth to bootstrap the session.
			const res = await fetch(`${PUBLIC_API_BASE}/v1/users/me`, {
				credentials: 'include'
			});
			if (!res.ok) throw new Error('Failed to load session');

			// Extract the access token from the cookie if available,
			// otherwise the cookie-based auth will work for API calls.
			// Store a marker so the auth store knows we're logged in.
			await setAuth('cookie-auth');
			goto('/');
		} catch (err: any) {
			error = err.message || 'Authentication failed';
		}
	});
</script>

<svelte:head>
	<title>Signing in… — Aileron</title>
</svelte:head>

<div class="flex items-center justify-center min-h-[70vh]">
	{#if error}
		<div class="text-center">
			<p class="text-sm text-destructive mb-4">{error}</p>
			<a href="/login" class="text-sm underline">Back to login</a>
		</div>
	{:else}
		<p class="text-sm text-muted-foreground">Signing in…</p>
	{/if}
</div>
