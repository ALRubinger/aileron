<script lang="ts">
	import { listApprovals } from '$lib/api';
	import { onMount } from 'svelte';
	import { approvalStatusColor } from '$lib/colors';
	import * as Card from '$lib/components/ui/card';

	let approvals = $state<any[]>([]);
	let loading = $state(true);
	let error = $state('');

	async function load() {
		try {
			const data = await listApprovals();
			approvals = data.items || [];
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		load();
		const interval = setInterval(load, 5000);
		return () => clearInterval(interval);
	});
</script>

<svelte:head>
	<title>Approvals - Aileron</title>
</svelte:head>

<h1 class="text-xl font-semibold mb-4">Approvals</h1>

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else if error}
	<p class="text-destructive">{error}</p>
{:else if approvals.length === 0}
	<p class="text-muted-foreground">No approval requests yet. Submit an intent via the API or MCP server.</p>
{:else}
	<div class="flex flex-col gap-3">
		{#each approvals as approval}
			<a href="/approvals/{approval.approval_id}" class="no-underline text-foreground">
				<Card.Root class="hover:bg-muted/50 transition-colors">
					<Card.Header>
						<div class="flex justify-between items-center">
							<div>
								<span class="font-semibold">{approval.approval_id}</span>
								{#if approval.rationale}
									<span class="text-muted-foreground ml-3 text-sm">{approval.rationale}</span>
								{/if}
							</div>
							<span class="font-semibold text-sm uppercase" style="color: {approvalStatusColor(approval.status)}">
								{approval.status}
							</span>
						</div>
					</Card.Header>
					<Card.Content>
						<div class="text-xs text-muted-foreground">
							Intent: {approval.intent_id} | Requested: {new Date(approval.requested_at).toLocaleString()}
						</div>
					</Card.Content>
				</Card.Root>
			</a>
		{/each}
	</div>
{/if}
