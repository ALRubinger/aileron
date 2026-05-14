<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import PassphraseModal from '$lib/components/PassphraseModal.svelte';
	import { vaultState, refreshVaultStatus } from '$lib/vault.svelte';

	// Local-webapp shell. Single tab focus: the daemon serves this on
	// the same origin the agent talks to, so there's no auth, no
	// multi-user header, no organization selector — just the bare
	// chrome that frames the approvals queue.
	//
	// Vault state hangs off this shell (#429): the modal opens
	// reactively when the vault is locked, and a small chip in the
	// nav shows the current state at a glance.
	let { children } = $props();

	const stateLabel = $derived(
		vaultState() === 'unlocked'
			? 'unlocked'
			: vaultState() === 'missing'
				? 'no vault'
				: 'locked'
	);
	const stateClass = $derived(
		vaultState() === 'unlocked'
			? 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-100'
			: 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-100'
	);

	onMount(() => {
		void refreshVaultStatus();
	});
</script>

<svelte:head>
	<title>Aileron</title>
</svelte:head>

<header class="border-b border-border bg-card shadow-low">
	<div class="mx-auto flex max-w-4xl items-center justify-between px-4 py-3">
		<a
			href="/"
			class="text-foreground no-underline hover:text-foreground"
		>
			<span class="font-extrabold text-2xl tracking-tight">Aileron</span><span
				class="font-normal text-2xl tracking-tight text-muted-foreground"
			>ControlPlane</span>
		</a>
		<nav class="flex items-center gap-4 text-sm">
			<span
				data-testid="vault-chip"
				class="rounded-full px-2 py-0.5 text-xs font-medium {stateClass}"
				title="Local vault state"
			>
				{stateLabel}
			</span>
			<a href="/actions" class="text-foreground hover:text-primary no-underline">Actions</a>
			<a href="/approvals" class="text-foreground hover:text-primary no-underline"
				>Approvals</a
			>
			<a href="/hub" class="text-foreground hover:text-primary no-underline">Hub</a>
		</nav>
	</div>
</header>

<main class="mx-auto max-w-4xl px-4 py-6">
	{@render children?.()}
</main>

<PassphraseModal />
