<script lang="ts">
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import { getVaultStatus } from '$lib/api';
	import { setupPassphrase, unlockVault, type UnlockProgress } from '$lib/crypto/vault.svelte.js';
	import { setSessionExpiresAt } from '$lib/vault.svelte.js';
	import { isAuthenticated } from '$lib/auth.svelte.js';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Button } from '$lib/components/ui/button';

	let step = $state<'setup' | 'unlock' | 'done'>('setup');
	let loading = $state(true);
	let error = $state('');
	let saving = $state(false);

	let passphrase = $state('');
	let confirm = $state('');

	let unlockPassphrase = $state('');
	let unlockLoading = $state(false);
	let unlockProgress = $state<UnlockProgress | null>(null);

	const unlockProgressLabels: Record<UnlockProgress, string> = {
		deriving: 'Deriving key...',
		verifying: 'Verifying passphrase...',
		attesting: 'Verifying enclave...',
		establishing: 'Establishing secure session...',
		done: 'Done'
	};

	onMount(async () => {
		if (!isAuthenticated()) {
			goto('/login?redirectTo=/setup-vault');
			return;
		}

		try {
			const status = await getVaultStatus();
			if (status.has_passphrase && !status.locked) {
				// Already set up and unlocked — go to dashboard.
				goto('/');
				return;
			}
			if (status.has_passphrase) {
				step = 'unlock';
			}
		} catch {
			// If vault status fails, show setup flow.
		} finally {
			loading = false;
		}
	});

	async function handleSetup(e: Event) {
		e.preventDefault();
		error = '';

		if (passphrase.length < 8) {
			error = 'Passphrase must be at least 8 characters';
			return;
		}
		if (passphrase !== confirm) {
			error = 'Passphrases do not match';
			return;
		}

		saving = true;
		try {
			await setupPassphrase(passphrase);
			// Move to unlock step (use same passphrase).
			unlockPassphrase = passphrase;
			passphrase = '';
			confirm = '';
			step = 'unlock';
			// Auto-unlock with the passphrase they just set.
			await doUnlock();
		} catch (e: any) {
			error = e.message;
		} finally {
			saving = false;
		}
	}

	async function handleUnlock(e: Event) {
		e.preventDefault();
		await doUnlock();
	}

	async function doUnlock() {
		error = '';
		unlockLoading = true;
		unlockProgress = null;
		try {
			const result = await unlockVault(unlockPassphrase, (s) => {
				unlockProgress = s;
			});
			if (result.valid) {
				if (result.sessionExpiresAt) {
					setSessionExpiresAt(result.sessionExpiresAt);
				}
				step = 'done';
				// Redirect to dashboard after brief success display.
				setTimeout(() => goto('/'), 1500);
			} else {
				error = 'Incorrect passphrase';
			}
		} catch (e: any) {
			error = e.message;
		} finally {
			unlockLoading = false;
			unlockProgress = null;
		}
	}
</script>

<svelte:head>
	<title>Set up vault — Aileron</title>
</svelte:head>

<div class="flex items-center justify-center min-h-[70vh]">
	{#if loading}
		<p class="text-muted-foreground">Loading...</p>
	{:else if step === 'setup'}
		<Card.Root class="w-full max-w-sm">
			<Card.Header>
				<Card.Title class="text-xl">Secure your vault</Card.Title>
				<Card.Description>
					Set a passphrase to encrypt your connected account credentials.
					This passphrase never leaves your device.
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<form onsubmit={handleSetup} class="flex flex-col gap-4">
					{#if error}
						<p class="text-sm text-destructive">{error}</p>
					{/if}

					<div class="flex flex-col gap-1.5">
						<label for="passphrase" class="text-sm font-medium">Vault passphrase</label>
						<Input
							id="passphrase"
							type="password"
							placeholder="At least 8 characters"
							bind:value={passphrase}
							disabled={saving}
							autofocus
						/>
					</div>

					<div class="flex flex-col gap-1.5">
						<label for="confirm" class="text-sm font-medium">Confirm passphrase</label>
						<Input
							id="confirm"
							type="password"
							placeholder="Re-enter your passphrase"
							bind:value={confirm}
							disabled={saving}
						/>
					</div>

					<p class="text-xs text-muted-foreground">
						Your passphrase derives an encryption key locally. Aileron never sees
						your passphrase or key. If you forget it, connected accounts must be
						re-linked.
					</p>

					<Button type="submit" disabled={saving} class="w-full">
						{saving ? 'Setting up...' : 'Continue'}
					</Button>
				</form>
			</Card.Content>
		</Card.Root>
	{:else if step === 'unlock'}
		<Card.Root class="w-full max-w-sm">
			<Card.Header>
				<Card.Title class="text-xl">Unlock your vault</Card.Title>
				<Card.Description>
					Enter your vault passphrase to unlock encrypted credentials.
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<form onsubmit={handleUnlock} class="flex flex-col gap-4">
					{#if error}
						<p class="text-sm text-destructive">{error}</p>
					{/if}

					<div class="flex flex-col gap-1.5">
						<label for="unlock_pass" class="text-sm font-medium">Passphrase</label>
						<Input
							id="unlock_pass"
							type="password"
							placeholder="Your vault passphrase"
							bind:value={unlockPassphrase}
							disabled={unlockLoading}
							autofocus
						/>
					</div>

					<Button type="submit" disabled={unlockLoading || !unlockPassphrase} class="w-full">
						{#if unlockProgress}
							{unlockProgressLabels[unlockProgress]}
						{:else}
							{unlockLoading ? 'Unlocking...' : 'Unlock'}
						{/if}
					</Button>
				</form>
			</Card.Content>
		</Card.Root>
	{:else if step === 'done'}
		<Card.Root class="w-full max-w-sm">
			<Card.Header>
				<Card.Title class="text-xl">Vault unlocked</Card.Title>
				<Card.Description>
					Your vault is set up and ready. Redirecting to dashboard...
				</Card.Description>
			</Card.Header>
		</Card.Root>
	{/if}
</div>
