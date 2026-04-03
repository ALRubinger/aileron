<script lang="ts">
	import { goto } from '$app/navigation';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import * as Card from '$lib/components/ui/card';
	import { emailLogin, setAuth, isAuthenticated } from '$lib/auth.js';
	import { PUBLIC_API_BASE } from '$env/static/public';
	import { onMount } from 'svelte';

	let email = $state('');
	let password = $state('');
	let error = $state('');
	let loading = $state(false);

	onMount(() => {
		if (isAuthenticated()) goto('/');
	});

	async function handleSubmit(e: Event) {
		e.preventDefault();
		error = '';
		loading = true;

		try {
			const data = await emailLogin(email, password);
			await setAuth(data.access_token);
			goto('/');
		} catch (err: any) {
			error = err.message || 'Login failed';
		} finally {
			loading = false;
		}
	}

	function handleGoogleLogin() {
		window.location.href = `${PUBLIC_API_BASE}/auth/google/login`;
	}
</script>

<svelte:head>
	<title>Log in — Aileron</title>
</svelte:head>

<div class="flex items-center justify-center min-h-[70vh]">
	<Card.Root class="w-full max-w-sm">
		<Card.Header>
			<Card.Title class="text-xl">Log in</Card.Title>
			<Card.Description>Sign in to your Aileron account</Card.Description>
		</Card.Header>
		<Card.Content>
			<form onsubmit={handleSubmit} class="flex flex-col gap-4">
				{#if error}
					<p class="text-sm text-destructive">{error}</p>
				{/if}

				<div class="flex flex-col gap-1.5">
					<label for="email" class="text-sm font-medium">Email</label>
					<Input
						id="email"
						type="email"
						placeholder="you@example.com"
						bind:value={email}
						required
					/>
				</div>

				<div class="flex flex-col gap-1.5">
					<label for="password" class="text-sm font-medium">Password</label>
					<Input
						id="password"
						type="password"
						placeholder="••••••••"
						bind:value={password}
						required
					/>
				</div>

				<Button type="submit" disabled={loading} class="w-full">
					{loading ? 'Signing in…' : 'Sign in'}
				</Button>
			</form>

			<div class="relative my-4">
				<div class="absolute inset-0 flex items-center">
					<div class="w-full border-t border-border"></div>
				</div>
				<div class="relative flex justify-center text-xs">
					<span class="bg-card px-2 text-muted-foreground">or</span>
				</div>
			</div>

			<Button variant="outline" class="w-full" onclick={handleGoogleLogin}>
				Sign in with Google
			</Button>
		</Card.Content>
		<Card.Footer class="justify-center">
			<p class="text-sm text-muted-foreground">
				Don't have an account? <a href="/signup" class="text-foreground underline underline-offset-4">Sign up</a>
			</p>
		</Card.Footer>
	</Card.Root>
</div>
