<script lang="ts">
	import { page } from '$app/stores';
	import { getApproval, getIntent, approveRequest, denyRequest } from '$lib/api';
	import { onMount } from 'svelte';
	import { approvalStatusColor, riskColor } from '$lib/colors';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';

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
</script>

<svelte:head>
	<title>Approval {id} - Aileron</title>
</svelte:head>

<a href="/approvals" class="text-muted-foreground no-underline text-sm">&larr; Back to approvals</a>

{#if loading}
	<p class="text-muted-foreground mt-4">Loading...</p>
{:else if error}
	<p class="text-destructive mt-4">{error}</p>
{:else if approval}
	<div class="mt-4">
		<div class="flex items-center gap-4 mb-6">
			<h1 class="text-xl font-semibold m-0">Approval Request</h1>
			<span class="font-semibold text-sm uppercase rounded border px-2.5 py-0.5" style="color: {approvalStatusColor(approval.status)}; border-color: {approvalStatusColor(approval.status)}">
				{approval.status}
			</span>
		</div>

		{#if intent}
			<Card.Root class="mb-4">
				<Card.Header>
					<Card.Title>Intent</Card.Title>
				</Card.Header>
				<Card.Content>
					<div class="grid grid-cols-[120px_1fr] gap-2 text-sm">
						<span class="text-muted-foreground">Action:</span>
						<span>{intent.action.type}</span>
						<span class="text-muted-foreground">Summary:</span>
						<span>{intent.action.summary}</span>
						{#if intent.action.justification}
							<span class="text-muted-foreground">Justification:</span>
							<span>{intent.action.justification}</span>
						{/if}
						<span class="text-muted-foreground">Agent:</span>
						<span>{intent.agent.id}</span>
						<span class="text-muted-foreground">Risk:</span>
						<span class="font-semibold uppercase" style="color: {riskColor(intent.decision.risk_level)}">{intent.decision.risk_level}</span>

						{#if intent.action.domain?.git}
							<span class="text-muted-foreground">Repository:</span>
							<span>{intent.action.domain.git.repository || '-'}</span>
							<span class="text-muted-foreground">Branch:</span>
							<span>{intent.action.domain.git.branch || '-'} &rarr; {intent.action.domain.git.base_branch || '-'}</span>
							{#if intent.action.domain.git.pr_title}
								<span class="text-muted-foreground">PR Title:</span>
								<span>{intent.action.domain.git.pr_title}</span>
							{/if}
						{/if}

						{#if intent.action.domain?.payment}
							<span class="text-muted-foreground">Vendor:</span>
							<span>{intent.action.domain.payment.vendor_name || '-'}</span>
							{#if intent.action.domain.payment.amount}
								<span class="text-muted-foreground">Amount:</span>
								<span>{(intent.action.domain.payment.amount.amount / 100).toFixed(2)} {intent.action.domain.payment.amount.currency}</span>
							{/if}
						{/if}
					</div>

					{#if intent.decision.matched_policies?.length}
						<div class="mt-4 border-t border-border pt-3">
							<div class="font-semibold text-sm mb-2">Matched Policies</div>
							{#each intent.decision.matched_policies as match}
								<div class="text-sm text-muted-foreground">
									{match.explanation || match.rule_id} <span class="opacity-50">({match.policy_id})</span>
								</div>
							{/each}
						</div>
					{/if}
				</Card.Content>
			</Card.Root>
		{/if}

		<Card.Root class="mb-4">
			<Card.Content>
				<div class="grid grid-cols-[120px_1fr] gap-2 text-sm">
					<span class="text-muted-foreground">ID:</span>
					<span class="font-mono text-sm">{approval.approval_id}</span>
					<span class="text-muted-foreground">Intent:</span>
					<span class="font-mono text-sm">{approval.intent_id}</span>
					<span class="text-muted-foreground">Requested:</span>
					<span>{new Date(approval.requested_at).toLocaleString()}</span>
					{#if approval.expires_at}
						<span class="text-muted-foreground">Expires:</span>
						<span>{new Date(approval.expires_at).toLocaleString()}</span>
					{/if}
					{#if approval.resolved_at}
						<span class="text-muted-foreground">Resolved:</span>
						<span>{new Date(approval.resolved_at).toLocaleString()}</span>
					{/if}
				</div>
			</Card.Content>
		</Card.Root>

		{#if approval.status === 'pending'}
			<Card.Root>
				<Card.Header>
					<Card.Title>Action</Card.Title>
				</Card.Header>
				<Card.Content>
					<div class="flex gap-3 items-end">
						<Button
							onclick={handleApprove}
							disabled={acting}
							class="bg-status-green text-black hover:bg-status-green/80"
						>
							{acting ? 'Processing...' : 'Approve'}
						</Button>
						<div class="flex-1 flex gap-2">
							<Input
								type="text"
								bind:value={denyReason}
								placeholder="Reason for denial..."
								class="flex-1"
							/>
							<Button
								variant="destructive"
								onclick={handleDeny}
								disabled={acting}
							>
								Deny
							</Button>
						</div>
					</div>
				</Card.Content>
			</Card.Root>
		{/if}

		{#if actionResult}
			<Card.Root class="mt-3">
				<Card.Content>
					{actionResult}
				</Card.Content>
			</Card.Root>
		{/if}
	</div>
{/if}
