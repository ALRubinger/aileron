<script lang="ts">
	import { listConnectedAccounts, deleteConnectedAccount } from '$lib/api';
	import { onMount } from 'svelte';
	import { getToken } from '$lib/auth.svelte.js';
	import { PUBLIC_API_BASE } from '$env/static/public';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';

	interface ProtectedAction {
		id: string;
		name: string;
		description: string;
		provider: string;
		icon: string;
	}

	const actions: ProtectedAction[] = [
		{
			id: 'gmail',
			name: 'Email (Gmail)',
			description: 'Send emails, read inbox, draft replies. Aileron owns execution — agents submit intents, never touch credentials.',
			provider: 'gmail',
			icon: '\u2709\uFE0F'
		},
		{
			id: 'google_calendar',
			name: 'Calendar (Google)',
			description: 'Schedule meetings, create events, send invites. Aileron enforces policies on external attendees and out-of-hours events.',
			provider: 'google_calendar',
			icon: '\uD83D\uDCC5'
		}
	];

	let connectedAccounts = $state<any[]>([]);
	let loading = $state(true);
	let error = $state('');
	let disconnecting = $state<string | null>(null);

	async function load() {
		try {
			const data = await listConnectedAccounts();
			connectedAccounts = data?.items || [];
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	function getConnectedAccount(provider: string) {
		return connectedAccounts.find((a: any) => a.provider === provider);
	}

	function handleConnect(provider: string) {
		// Redirect to the OAuth connect flow on the backend.
		window.location.href = `${PUBLIC_API_BASE}/v1/connect/${provider}`;
	}

	async function handleDisconnect(accountId: string) {
		if (!confirm('Disconnect this account? Agents will no longer be able to execute actions through it.')) return;
		disconnecting = accountId;
		try {
			await deleteConnectedAccount(accountId);
			await load();
		} catch (e: any) {
			error = e.message;
		} finally {
			disconnecting = null;
		}
	}

	onMount(() => {
		load();
	});
</script>

<svelte:head>
	<title>Protected Actions - Aileron</title>
</svelte:head>

<div class="mb-6">
	<h1 class="text-xl font-semibold mb-1">Protected Actions</h1>
	<p class="text-sm text-muted-foreground">
		Irreversible actions that Aileron owns and executes on your behalf. Connect your accounts to enable agents to submit intents through Aileron.
	</p>
</div>

{#if error}
	<p class="text-destructive text-sm mb-4">{error}</p>
{/if}

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else}
	<div class="grid grid-cols-[repeat(auto-fill,minmax(300px,1fr))] gap-4">
		{#each actions as action}
			{@const connected = getConnectedAccount(action.provider)}
			<Card.Root class="flex flex-col">
				<Card.Header class="flex-1">
					<Card.Title>
						<span class="mr-2">{action.icon}</span>
						{action.name}
					</Card.Title>
					<Card.Description>{action.description}</Card.Description>
				</Card.Header>
				<Card.Content>
					{#if connected}
						<div class="flex items-center justify-between">
							<div class="flex items-center gap-2">
								<Badge variant="outline" class="text-green-500 border-green-500">Connected</Badge>
								<span class="text-xs text-muted-foreground">{connected.email}</span>
							</div>
							<Button
								variant="outline"
								size="xs"
								onclick={() => handleDisconnect(connected.id)}
								disabled={disconnecting === connected.id}
							>
								{disconnecting === connected.id ? 'Disconnecting...' : 'Disconnect'}
							</Button>
						</div>
					{:else}
						<Button
							size="sm"
							onclick={() => handleConnect(action.provider)}
						>
							Connect
						</Button>
					{/if}
				</Card.Content>
			</Card.Root>
		{/each}
	</div>
{/if}
