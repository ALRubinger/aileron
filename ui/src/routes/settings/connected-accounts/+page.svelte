<script lang="ts">
	import { onMount } from 'svelte';
	import { PUBLIC_API_BASE } from '$env/static/public';
	import { listConnectedAccounts, deleteConnectedAccount, getVaultStatus } from '$lib/api';
	import { setupPassphrase, unlockVault, type UnlockProgress } from '$lib/crypto/vault.svelte.js';
	import { setSessionExpiresAt } from '$lib/vault.svelte.js';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { Input } from '$lib/components/ui/input';
	import * as Dialog from '$lib/components/ui/dialog';

	interface ConnectedAccount {
		id: string;
		provider: string;
		external_user_id: string;
		status: string;
		scopes: string[];
		created_at: string;
		updated_at: string;
	}

	const providerMeta: Record<string, { name: string; description: string }> = {
		slack: { name: 'Slack', description: 'Channels, messages, and search' },
		github_repos: { name: 'GitHub', description: 'Repositories, PRs, and code search' },
		gmail: { name: 'Gmail', description: 'Email reading and sending' },
		google_calendar: { name: 'Google Calendar', description: 'Calendar events and scheduling' }
	};

	let accounts = $state<ConnectedAccount[]>([]);
	let loading = $state(true);
	let error = $state('');
	let success = $state('');
	let disconnecting = $state('');
	let confirmAccount = $state<ConnectedAccount | null>(null);
	let confirmOpen = $state(false);

	// Vault state.
	let vaultStatus = $state<{ locked: boolean; has_passphrase: boolean; expires_at?: string } | null>(null);
	let showSetupForm = $state(false);
	let setupPassphraseValue = $state('');
	let setupConfirmValue = $state('');
	let setupError = $state('');
	let setupLoading = $state(false);
	let unlockPassphrase = $state('');
	let unlockError = $state('');
	let unlockLoading = $state(false);
	let unlockProgress = $state<UnlockProgress | null>(null);

	const unlockProgressLabels: Record<UnlockProgress, string> = {
		deriving: 'Deriving key...',
		verifying: 'Verifying passphrase...',
		attesting: 'Verifying enclave...',
		establishing: 'Establishing secure session...',
		done: 'Done'
	};

	const connectedProviders = $derived(new Set(accounts.map((a) => a.provider)));
	const availableProviders = $derived(
		Object.keys(providerMeta).filter((p) => !connectedProviders.has(p))
	);

	const vaultReady = $derived(
		vaultStatus !== null && vaultStatus.has_passphrase && !vaultStatus.locked
	);

	onMount(async () => {
		await load();
	});

	async function load() {
		try {
			const [accountData, status] = await Promise.all([
				listConnectedAccounts(),
				getVaultStatus().catch(() => null)
			]);
			accounts = accountData.items ?? accountData ?? [];
			vaultStatus = status;
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	function handleConnect(provider: string) {
		const returnTo = `${window.location.origin}/settings/connected-accounts`;
		window.location.href = `${PUBLIC_API_BASE}/v1/connect/${provider}?return_to=${encodeURIComponent(returnTo)}`;
	}

	function openDisconnectDialog(account: ConnectedAccount) {
		confirmAccount = account;
		confirmOpen = true;
	}

	async function handleDisconnect() {
		if (!confirmAccount) return;
		const id = confirmAccount.id;
		const name = providerMeta[confirmAccount.provider]?.name ?? confirmAccount.provider;
		error = '';
		disconnecting = id;
		try {
			await deleteConnectedAccount(id);
			confirmOpen = false;
			confirmAccount = null;
			success = `${name} disconnected`;
			setTimeout(() => (success = ''), 3000);
			await load();
		} catch (e: any) {
			error = e.message;
		} finally {
			disconnecting = '';
		}
	}

	async function handleSetupPassphrase() {
		setupError = '';
		if (setupPassphraseValue.length < 8) {
			setupError = 'Passphrase must be at least 8 characters';
			return;
		}
		if (setupPassphraseValue !== setupConfirmValue) {
			setupError = 'Passphrases do not match';
			return;
		}

		setupLoading = true;
		try {
			await setupPassphrase(setupPassphraseValue);
			setupPassphraseValue = '';
			setupConfirmValue = '';
			showSetupForm = false;
			// After setup, immediately unlock.
			vaultStatus = { locked: true, has_passphrase: true };
			success = 'Vault passphrase set. Now unlock to connect accounts.';
			setTimeout(() => (success = ''), 5000);
		} catch (e: any) {
			setupError = e.message;
		} finally {
			setupLoading = false;
		}
	}

	async function handleUnlock() {
		unlockError = '';
		unlockLoading = true;
		unlockProgress = null;
		try {
			const result = await unlockVault(unlockPassphrase, (step) => {
				unlockProgress = step;
			});
			if (result.valid) {
				if (result.sessionExpiresAt) {
					setSessionExpiresAt(result.sessionExpiresAt);
				}
				unlockPassphrase = '';
				unlockProgress = null;
				vaultStatus = { locked: false, has_passphrase: true };
				success = 'Vault unlocked';
				setTimeout(() => (success = ''), 3000);
			} else {
				unlockError = 'Incorrect passphrase';
			}
		} catch (e: any) {
			unlockError = e.message;
		} finally {
			unlockLoading = false;
			unlockProgress = null;
		}
	}

	function formatDate(d: string) {
		return new Date(d).toLocaleDateString(undefined, {
			year: 'numeric',
			month: 'long',
			day: 'numeric',
			timeZone: 'UTC'
		});
	}

	function statusBadgeClass(status: string): string {
		switch (status) {
			case 'active':
				return 'border-green-500/50 text-green-600';
			case 'expired':
				return 'border-yellow-500/50 text-yellow-600';
			case 'revoked':
				return 'border-red-500/50 text-red-600';
			default:
				return '';
		}
	}
</script>

<svelte:head>
	<title>Connected Accounts - Aileron</title>
</svelte:head>

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else}
	<div class="flex flex-col gap-6">
		{#if error}
			<p class="text-sm text-destructive">{error}</p>
		{/if}
		{#if success}
			<p class="text-sm text-green-600">{success}</p>
		{/if}

		{#if vaultStatus && !vaultStatus.has_passphrase}
			<Card.Root class="border-yellow-500/50">
				<Card.Header>
					<Card.Title>Set up vault passphrase</Card.Title>
					<Card.Description>
						A vault passphrase is required to encrypt your connected account tokens.
						Your passphrase never leaves this device.
					</Card.Description>
				</Card.Header>
				<Card.Content>
					{#if showSetupForm}
						<form onsubmit={(e) => { e.preventDefault(); handleSetupPassphrase(); }} class="flex flex-col gap-4">
							<div class="flex flex-col gap-1.5">
								<label for="setup_passphrase" class="text-sm font-medium">Passphrase</label>
								<Input
									id="setup_passphrase"
									type="password"
									bind:value={setupPassphraseValue}
									placeholder="At least 8 characters"
									class="max-w-sm"
									disabled={setupLoading}
								/>
							</div>
							<div class="flex flex-col gap-1.5">
								<label for="setup_confirm" class="text-sm font-medium">Confirm passphrase</label>
								<Input
									id="setup_confirm"
									type="password"
									bind:value={setupConfirmValue}
									placeholder="Re-enter your passphrase"
									class="max-w-sm"
									disabled={setupLoading}
								/>
							</div>
							{#if setupError}
								<p class="text-sm text-destructive">{setupError}</p>
							{/if}
							<div class="flex gap-3">
								<Button type="submit" size="sm" disabled={setupLoading}>
									{setupLoading ? 'Setting up...' : 'Set passphrase'}
								</Button>
								<Button variant="outline" size="sm" type="button" onclick={() => (showSetupForm = false)}>
									Cancel
								</Button>
							</div>
						</form>
					{:else}
						<Button size="sm" onclick={() => (showSetupForm = true)}>
							Set vault passphrase
						</Button>
					{/if}
				</Card.Content>
			</Card.Root>
		{:else if vaultStatus && vaultStatus.locked}
			<Card.Root class="border-yellow-500/50">
				<Card.Header>
					<Card.Title>Vault locked</Card.Title>
					<Card.Description>
						Unlock your vault to connect or manage accounts. Your passphrase is used
						locally to derive the encryption key.
					</Card.Description>
				</Card.Header>
				<Card.Content>
					<form onsubmit={(e) => { e.preventDefault(); handleUnlock(); }} class="flex flex-col gap-4">
						<div class="flex flex-col gap-1.5">
							<label for="unlock_passphrase" class="text-sm font-medium">Passphrase</label>
							<Input
								id="unlock_passphrase"
								type="password"
								bind:value={unlockPassphrase}
								placeholder="Enter your vault passphrase"
								class="max-w-sm"
								disabled={unlockLoading}
								autofocus
							/>
						</div>
						{#if unlockError}
							<p class="text-sm text-destructive">{unlockError}</p>
						{/if}
						<div>
							<Button type="submit" size="sm" disabled={unlockLoading || !unlockPassphrase}>
								{#if unlockProgress}
									{unlockProgressLabels[unlockProgress]}
								{:else}
									{unlockLoading ? 'Unlocking...' : 'Unlock'}
								{/if}
							</Button>
						</div>
					</form>
				</Card.Content>
			</Card.Root>
		{/if}

		<Card.Root>
			<Card.Header>
				<Card.Title>Connected Accounts</Card.Title>
				<Card.Description>
					External services linked to your Aileron account
				</Card.Description>
			</Card.Header>
			<Card.Content>
				{#if accounts.length === 0}
					<p class="text-sm text-muted-foreground">No accounts connected yet.</p>
				{:else}
					<div class="flex flex-col gap-4">
						{#each accounts as account}
							<div class="flex items-center justify-between">
								<div class="flex flex-col gap-1">
									<div class="flex items-center gap-2">
										<span class="text-sm font-medium">
											{providerMeta[account.provider]?.name ?? account.provider}
										</span>
										<Badge variant="outline" class={statusBadgeClass(account.status)}>
											{account.status}
										</Badge>
									</div>
									<span class="text-sm text-muted-foreground">
										{account.external_user_id}
									</span>
									<span class="text-xs text-muted-foreground">
										Connected {formatDate(account.created_at)}
									</span>
								</div>
								<Button
									size="sm"
									variant="ghost"
									class="text-destructive hover:text-destructive"
									onclick={() => openDisconnectDialog(account)}
								>
									Disconnect
								</Button>
							</div>
						{/each}
					</div>
				{/if}
			</Card.Content>
		</Card.Root>

		{#if availableProviders.length > 0}
			<Card.Root>
				<Card.Header>
					<Card.Title>Available Integrations</Card.Title>
					<Card.Description>
						Connect additional services to enhance Aileron
					</Card.Description>
				</Card.Header>
				<Card.Content>
					{#if !vaultReady && vaultStatus}
						<p class="text-sm text-muted-foreground mb-4">
							{vaultStatus.has_passphrase
								? 'Unlock your vault above to connect new accounts.'
								: 'Set up your vault passphrase above to connect accounts.'}
						</p>
					{/if}
					<div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
						{#each availableProviders as provider}
							{@const meta = providerMeta[provider]}
							<div
								class="flex items-center justify-between rounded-lg border p-4"
							>
								<div class="flex flex-col gap-0.5">
									<span class="text-sm font-medium">{meta.name}</span>
									<span class="text-xs text-muted-foreground">
										{meta.description}
									</span>
								</div>
								<Button
									size="sm"
									onclick={() => handleConnect(provider)}
									disabled={!vaultReady}
								>
									Connect
								</Button>
							</div>
						{/each}
					</div>
				</Card.Content>
			</Card.Root>
		{/if}
	</div>

	<Dialog.Root bind:open={confirmOpen}>
		<Dialog.Content>
			<Dialog.Header>
				<Dialog.Title>
					Disconnect {confirmAccount
						? (providerMeta[confirmAccount.provider]?.name ?? confirmAccount.provider)
						: ''}?
				</Dialog.Title>
				<Dialog.Description>
					This will remove the connection and revoke Aileron's access. You can reconnect at any
					time.
				</Dialog.Description>
			</Dialog.Header>
			<Dialog.Footer>
				<Button variant="outline" onclick={() => (confirmOpen = false)}>Cancel</Button>
				<Button
					variant="destructive"
					disabled={!!disconnecting}
					onclick={handleDisconnect}
				>
					{disconnecting ? 'Disconnecting...' : 'Disconnect'}
				</Button>
			</Dialog.Footer>
		</Dialog.Content>
	</Dialog.Root>
{/if}
