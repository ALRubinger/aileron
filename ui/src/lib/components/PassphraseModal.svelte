<script lang="ts">
	import {
		Dialog,
		DialogContent,
		DialogHeader,
		DialogTitle,
		DialogDescription,
		DialogFooter
	} from '$lib/components/ui/dialog';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { isVaultLocked, onUnlocked, dismissUnlock, setSessionExpiresAt } from '$lib/vault.svelte.js';
	import { unlockVault, type UnlockProgress } from '$lib/crypto/vault.svelte.js';

	let passphrase = $state('');
	let error = $state('');
	let loading = $state(false);
	let progressStep = $state<UnlockProgress | null>(null);

	let open = $derived(isVaultLocked());

	const progressLabels: Record<UnlockProgress, string> = {
		deriving: 'Deriving key...',
		verifying: 'Verifying passphrase...',
		establishing: 'Establishing secure session...',
		done: 'Done'
	};

	let statusText = $derived(
		progressStep ? progressLabels[progressStep] : (loading ? 'Unlocking...' : 'Unlock')
	);

	async function handleSubmit(e: Event) {
		e.preventDefault();
		error = '';
		loading = true;
		progressStep = null;

		try {
			const result = await unlockVault(passphrase, (step) => {
				progressStep = step;
			});
			if (result.valid) {
				if (result.sessionExpiresAt) {
					setSessionExpiresAt(result.sessionExpiresAt);
				}
				passphrase = '';
				progressStep = null;
				await onUnlocked();
			} else {
				error = 'Incorrect passphrase';
			}
		} catch (err) {
			error = err instanceof Error ? err.message : 'Verification failed';
		} finally {
			loading = false;
			progressStep = null;
		}
	}

	function handleCancel() {
		passphrase = '';
		error = '';
		progressStep = null;
		dismissUnlock();
	}

	function handleOpenChange(value: boolean) {
		if (!value) {
			handleCancel();
		}
	}
</script>

<Dialog {open} onOpenChange={handleOpenChange}>
	<DialogContent>
		<DialogHeader>
			<DialogTitle>Unlock Vault</DialogTitle>
			<DialogDescription>
				Enter your vault passphrase to unlock encrypted credentials. Your passphrase never leaves this device.
			</DialogDescription>
		</DialogHeader>

		<form onsubmit={handleSubmit} class="flex flex-col gap-4 mt-4">
			<Input
				type="password"
				placeholder="Vault passphrase"
				bind:value={passphrase}
				disabled={loading}
				autofocus
			/>

			{#if error}
				<p class="text-sm text-destructive">{error}</p>
			{/if}

			<DialogFooter>
				<Button variant="outline" type="button" onclick={handleCancel} disabled={loading}>
					Cancel
				</Button>
				<Button type="submit" disabled={loading || !passphrase}>
					{statusText}
				</Button>
			</DialogFooter>
		</form>
	</DialogContent>
</Dialog>
