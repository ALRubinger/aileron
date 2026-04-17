<script lang="ts">
	import { onMount } from 'svelte';
	import { PUBLIC_API_BASE } from '$env/static/public';
	import { listConnectedAccounts, deleteConnectedAccount } from '$lib/api';
	import { connectedAccountStatusColor } from '$lib/colors';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import * as Dialog from '$lib/components/ui/dialog';

	interface ConnectedAccount {
		id: string;
		provider: string;
		email: string;
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

	const connectedProviders = $derived(new Set(accounts.map((a) => a.provider)));
	const availableProviders = $derived(
		Object.keys(providerMeta).filter((p) => !connectedProviders.has(p))
	);

	onMount(async () => {
		await load();
	});

	async function load() {
		try {
			const data = await listConnectedAccounts();
			accounts = data.items ?? data ?? [];
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	function handleConnect(provider: string) {
		window.location.href = `${PUBLIC_API_BASE}/v1/connect/${provider}`;
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

	function formatDate(d: string) {
		return new Date(d).toLocaleDateString(undefined, {
			year: 'numeric',
			month: 'long',
			day: 'numeric'
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
										{account.email}
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
								<Button size="sm" onclick={() => handleConnect(provider)}>
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
