<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import * as Card from '$lib/components/ui/card';
	import { verifyEmail } from '$lib/auth.svelte.js';

	let email = $state(page.url.searchParams.get('email') || '');
	let code = $state('');
	let error = $state('');
	let success = $state(false);
	let loading = $state(false);

	async function handleSubmit(e: Event) {
		e.preventDefault();
		error = '';
		loading = true;

		try {
			await verifyEmail(email, code);
			success = true;
		} catch (err: any) {
			error = err.message || 'Verification failed';
		} finally {
			loading = false;
		}
	}
</script>

<svelte:head>
	<title>Verify email — Aileron</title>
</svelte:head>

<div class="flex items-center justify-center min-h-[70vh]">
	<Card.Root class="w-full max-w-sm">
		<Card.Header>
			<Card.Title class="text-xl">Verify your email</Card.Title>
			<Card.Description>
				Enter the 6-digit code sent to {email || 'your email'}
			</Card.Description>
		</Card.Header>
		<Card.Content>
			{#if success}
				<div class="flex flex-col gap-4 items-center text-center">
					<p class="text-sm text-muted-foreground">
						Your email has been verified. You can now log in.
					</p>
					<Button class="w-full" onclick={() => goto('/login')}>
						Go to login
					</Button>
				</div>
			{:else}
				<form onsubmit={handleSubmit} class="flex flex-col gap-4">
					{#if error}
						<p class="text-sm text-destructive">{error}</p>
					{/if}

					{#if !page.url.searchParams.get('email')}
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
					{/if}

					<div class="flex flex-col gap-1.5">
						<label for="code" class="text-sm font-medium">Verification code</label>
						<Input
							id="code"
							type="text"
							placeholder="000000"
							bind:value={code}
							maxlength={6}
							class="text-center text-lg tracking-widest"
							required
						/>
					</div>

					<Button type="submit" disabled={loading} class="w-full">
						{loading ? 'Verifying…' : 'Verify'}
					</Button>
				</form>
			{/if}
		</Card.Content>
	</Card.Root>
</div>
