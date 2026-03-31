<script lang="ts">
	import { page } from '$app/stores';
	import { getApproval, getIntent, approveRequest, denyRequest } from '$lib/api';
	import { onMount } from 'svelte';

	let approval = $state<any>(null);
	let intent = $state<any>(null);
	let loading = $state(true);
	let error = $state('');
	let actionResult = $state('');
	let acting = $state(false);
	let denyReason = $state('');

	const id = $derived($page.params.id ?? '');

	async function load() {
		if (!id) return;
		try {
			approval = await getApproval(id);
			if (approval.intent_id) {
				intent = await getIntent(approval.intent_id);
			}
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	onMount(() => { load(); });

	async function handleApprove() {
		acting = true;
		try {
			const result = await approveRequest(id);
			actionResult = `Approved. Grant ID: ${result.execution_grant_id}`;
			await load();
		} catch (e: any) {
			actionResult = `Error: ${e.message}`;
		} finally {
			acting = false;
		}
	}

	async function handleDeny() {
		if (!denyReason.trim()) {
			actionResult = 'Please provide a reason for denial.';
			return;
		}
		acting = true;
		try {
			await denyRequest(id, denyReason);
			actionResult = 'Denied.';
			await load();
		} catch (e: any) {
			actionResult = `Error: ${e.message}`;
		} finally {
			acting = false;
		}
	}

	function statusColor(status: string) {
		switch (status) {
			case 'pending': return 'var(--yellow)';
			case 'approved': return 'var(--green)';
			case 'denied': return 'var(--red)';
			case 'modified': return 'var(--orange)';
			default: return 'var(--text-muted)';
		}
	}

	function riskColor(level: string) {
		switch (level) {
			case 'low': return 'var(--green)';
			case 'medium': return 'var(--yellow)';
			case 'high': return 'var(--orange)';
			case 'critical': return 'var(--red)';
			default: return 'var(--text-muted)';
		}
	}
</script>

<svelte:head>
	<title>Approval {id} - Aileron</title>
</svelte:head>

<a href="/approvals" style="color: var(--text-muted); text-decoration: none; font-size: 0.85rem;">&larr; Back to approvals</a>

{#if loading}
	<p style="color: var(--text-muted); margin-top: 1rem;">Loading...</p>
{:else if error}
	<p style="color: var(--red); margin-top: 1rem;">{error}</p>
{:else if approval}
	<div style="margin-top: 1rem;">
		<div style="display: flex; align-items: center; gap: 1rem; margin-bottom: 1.5rem;">
			<h1 style="font-size: 1.3rem; margin: 0;">Approval Request</h1>
			<span style="color: {statusColor(approval.status)}; font-weight: 600; font-size: 0.9rem; text-transform: uppercase; padding: 0.2rem 0.6rem; border: 1px solid {statusColor(approval.status)}; border-radius: 4px;">
				{approval.status}
			</span>
		</div>

		<!-- Intent details -->
		{#if intent}
			<div style="border: 1px solid var(--border); border-radius: 8px; padding: 1rem; background: var(--bg-card); margin-bottom: 1rem;">
				<div style="font-weight: 600; margin-bottom: 0.75rem;">Intent</div>
				<div style="display: grid; grid-template-columns: 120px 1fr; gap: 0.5rem; font-size: 0.9rem;">
					<span style="color: var(--text-muted);">Action:</span>
					<span>{intent.action.type}</span>
					<span style="color: var(--text-muted);">Summary:</span>
					<span>{intent.action.summary}</span>
					{#if intent.action.justification}
						<span style="color: var(--text-muted);">Justification:</span>
						<span>{intent.action.justification}</span>
					{/if}
					<span style="color: var(--text-muted);">Agent:</span>
					<span>{intent.agent.id}</span>
					<span style="color: var(--text-muted);">Risk:</span>
					<span style="color: {riskColor(intent.decision.risk_level)}; font-weight: 600; text-transform: uppercase;">{intent.decision.risk_level}</span>

					{#if intent.action.domain?.git}
						<span style="color: var(--text-muted);">Repository:</span>
						<span>{intent.action.domain.git.repository || '-'}</span>
						<span style="color: var(--text-muted);">Branch:</span>
						<span>{intent.action.domain.git.branch || '-'} &rarr; {intent.action.domain.git.base_branch || '-'}</span>
						{#if intent.action.domain.git.pr_title}
							<span style="color: var(--text-muted);">PR Title:</span>
							<span>{intent.action.domain.git.pr_title}</span>
						{/if}
					{/if}

					{#if intent.action.domain?.payment}
						<span style="color: var(--text-muted);">Vendor:</span>
						<span>{intent.action.domain.payment.vendor_name || '-'}</span>
						{#if intent.action.domain.payment.amount}
							<span style="color: var(--text-muted);">Amount:</span>
							<span>{(intent.action.domain.payment.amount.amount / 100).toFixed(2)} {intent.action.domain.payment.amount.currency}</span>
						{/if}
					{/if}
				</div>

				{#if intent.decision.matched_policies?.length}
					<div style="margin-top: 1rem; border-top: 1px solid var(--border); padding-top: 0.75rem;">
						<div style="font-weight: 600; font-size: 0.85rem; margin-bottom: 0.5rem;">Matched Policies</div>
						{#each intent.decision.matched_policies as match}
							<div style="font-size: 0.85rem; color: var(--text-muted);">
								{match.explanation || match.rule_id} <span style="opacity: 0.5;">({match.policy_id})</span>
							</div>
						{/each}
					</div>
				{/if}
			</div>
		{/if}

		<!-- Approval metadata -->
		<div style="border: 1px solid var(--border); border-radius: 8px; padding: 1rem; background: var(--bg-card); margin-bottom: 1rem;">
			<div style="display: grid; grid-template-columns: 120px 1fr; gap: 0.5rem; font-size: 0.9rem;">
				<span style="color: var(--text-muted);">ID:</span>
				<span style="font-family: monospace; font-size: 0.85rem;">{approval.approval_id}</span>
				<span style="color: var(--text-muted);">Intent:</span>
				<span style="font-family: monospace; font-size: 0.85rem;">{approval.intent_id}</span>
				<span style="color: var(--text-muted);">Requested:</span>
				<span>{new Date(approval.requested_at).toLocaleString()}</span>
				{#if approval.expires_at}
					<span style="color: var(--text-muted);">Expires:</span>
					<span>{new Date(approval.expires_at).toLocaleString()}</span>
				{/if}
				{#if approval.resolved_at}
					<span style="color: var(--text-muted);">Resolved:</span>
					<span>{new Date(approval.resolved_at).toLocaleString()}</span>
				{/if}
			</div>
		</div>

		<!-- Action buttons -->
		{#if approval.status === 'pending'}
			<div style="border: 1px solid var(--border); border-radius: 8px; padding: 1rem; background: var(--bg-card);">
				<div style="font-weight: 600; margin-bottom: 0.75rem;">Action</div>
				<div style="display: flex; gap: 0.75rem; align-items: flex-end;">
					<button
						onclick={handleApprove}
						disabled={acting}
						style="padding: 0.5rem 1.5rem; background: var(--green); color: #000; border: none; border-radius: 6px; font-weight: 600; cursor: pointer; font-size: 0.9rem;"
					>
						{acting ? 'Processing...' : 'Approve'}
					</button>
					<div style="flex: 1; display: flex; gap: 0.5rem;">
						<input
							type="text"
							bind:value={denyReason}
							placeholder="Reason for denial..."
							style="flex: 1; padding: 0.5rem; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 0.9rem;"
						/>
						<button
							onclick={handleDeny}
							disabled={acting}
							style="padding: 0.5rem 1.5rem; background: var(--red); color: #fff; border: none; border-radius: 6px; font-weight: 600; cursor: pointer; font-size: 0.9rem;"
						>
							Deny
						</button>
					</div>
				</div>
			</div>
		{/if}

		{#if actionResult}
			<div style="margin-top: 0.75rem; padding: 0.75rem; border-radius: 6px; background: var(--bg-card); border: 1px solid var(--border); font-size: 0.9rem;">
				{actionResult}
			</div>
		{/if}
	</div>
{/if}
