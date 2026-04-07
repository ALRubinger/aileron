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
	import { verifyPassphrase } from '$lib/api';

	let passphrase = $state('');
	let error = $state('');
	let loading = $state(false);

	let open = $derived(isVaultLocked());

	async function handleSubmit(e: Event) {
		e.preventDefault();
		error = '';
		loading = true;

		try {
			const resp = await verifyPassphrase(passphrase);
			if (resp.valid) {
				if (resp.session_expires_at) {
					setSessionExpiresAt(new Date(resp.session_expires_at));
				}
				passphrase = '';
				await onUnlocked();
			} else {
				error = 'Incorrect passphrase';
			}
		} catch (err) {
			error = err instanceof Error ? err.message : 'Verification failed';
		} finally {
			loading = false;
		}
	}

	function handleCancel() {
		passphrase = '';
		error = '';
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
				Enter your vault passphrase to unlock encrypted credentials.
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
					{loading ? 'Unlocking...' : 'Unlock'}
				</Button>
			</DialogFooter>
		</form>
	</DialogContent>
</Dialog>
