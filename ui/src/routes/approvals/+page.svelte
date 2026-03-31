<script lang="ts">
	import { listApprovals } from '$lib/api';
	import { onMount } from 'svelte';

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

	function statusColor(status: string) {
		switch (status) {
			case 'pending': return 'var(--yellow)';
			case 'approved': return 'var(--green)';
			case 'denied': return 'var(--red)';
			case 'modified': return 'var(--orange)';
			default: return 'var(--text-muted)';
		}
	}
</script>

<svelte:head>
	<title>Approvals - Aileron</title>
</svelte:head>

<h1 style="font-size: 1.3rem; margin-bottom: 1rem;">Approvals</h1>

{#if loading}
	<p style="color: var(--text-muted);">Loading...</p>
{:else if error}
	<p style="color: var(--red);">{error}</p>
{:else if approvals.length === 0}
	<p style="color: var(--text-muted);">No approval requests yet. Submit an intent via the API or MCP server.</p>
{:else}
	<div style="display: flex; flex-direction: column; gap: 0.75rem;">
		{#each approvals as approval}
			<a href="/approvals/{approval.approval_id}" style="display: block; padding: 1rem; border: 1px solid var(--border); border-radius: 8px; text-decoration: none; color: var(--text); background: var(--bg-card);">
				<div style="display: flex; justify-content: space-between; align-items: center;">
					<div>
						<span style="font-weight: 600;">{approval.approval_id}</span>
						{#if approval.rationale}
							<span style="color: var(--text-muted); margin-left: 0.75rem; font-size: 0.85rem;">{approval.rationale}</span>
						{/if}
					</div>
					<span style="color: {statusColor(approval.status)}; font-weight: 600; font-size: 0.85rem; text-transform: uppercase;">
						{approval.status}
					</span>
				</div>
				<div style="margin-top: 0.5rem; font-size: 0.8rem; color: var(--text-muted);">
					Intent: {approval.intent_id} | Requested: {new Date(approval.requested_at).toLocaleString()}
				</div>
			</a>
		{/each}
	</div>
{/if}
