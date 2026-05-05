<script lang="ts">
	import * as Dialog from '$lib/components/ui/dialog';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { unlockLocalVault } from '$lib/api';
	import {
		isVaultLocked,
		vaultState,
		onVaultUnlocked,
		onVaultDismissed
	} from '$lib/vault.svelte';

	// Passphrase modal — opened whenever the daemon reports the local
	// vault as locked. The daemon's /v1/vault/unlock takes the
	// passphrase in plaintext over the loopback connection (single
	// trust boundary; see ADR-0011).

	let passphrase = $state('');
	let submitting = $state(false);
	let errorMessage = $state('');

	// Derived `open` so the dialog stays in sync with the central
	// vault store. Also gives us a place to clear the passphrase
	// buffer when the modal closes — leaving it in component state
	// across opens would leak the previous attempt.
	const open = $derived(isVaultLocked());

	function handleOpenChange(next: boolean) {
		if (!next) {
			passphrase = '';
			errorMessage = '';
			onVaultDismissed();
		}
	}

	async function handleSubmit(event: Event) {
		event.preventDefault();
		if (!passphrase || submitting) return;
		submitting = true;
		errorMessage = '';
		try {
			await unlockLocalVault(passphrase);
			passphrase = '';
			await onVaultUnlocked();
		} catch (err) {
			errorMessage =
				err instanceof Error ? err.message : 'failed to unlock the vault';
		} finally {
			submitting = false;
		}
	}
</script>

<Dialog.Root {open} onOpenChange={handleOpenChange}>
	<Dialog.Content>
		<Dialog.Header>
			<Dialog.Title>Unlock Aileron vault</Dialog.Title>
			<Dialog.Description>
				{#if vaultState() === 'missing'}
					No vault file was found at <code>~/.aileron/secrets.json</code>.
					Run <code>aileron launch</code> with
					<code>AILERON_VAULT_PROMPT=stderr</code> once to create one.
				{:else}
					Enter your vault passphrase. The daemon needs it to resolve
					credentials for any tool call the agent makes.
				{/if}
			</Dialog.Description>
		</Dialog.Header>

		{#if vaultState() !== 'missing'}
			<form onsubmit={handleSubmit} class="space-y-3 pt-2">
				<Input
					type="password"
					autocomplete="current-password"
					placeholder="vault passphrase"
					bind:value={passphrase}
					disabled={submitting}
				/>
				{#if errorMessage}
					<p class="text-sm text-destructive">{errorMessage}</p>
				{/if}
				<Dialog.Footer>
					<Button type="submit" disabled={submitting || !passphrase}>
						{submitting ? 'Unlocking…' : 'Unlock'}
					</Button>
				</Dialog.Footer>
			</form>
		{/if}
	</Dialog.Content>
</Dialog.Root>
