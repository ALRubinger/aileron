<script lang="ts">
	import {
		decideActionApproval,
		watchActionApprovals,
		type PendingActionApproval
	} from '$lib/api';
	import { onMount } from 'svelte';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';

	// Action-level approvals (#418): runtime-blocking yes/no for actions
	// whose manifest declared `[approval] required = true`. Each entry
	// represents one held-open `RunAction` HTTP response. The local
	// webapp surface drops the rich-governance section — that's
	// cloud-tier and lives in ../../ui/.
	let actionApprovals = $state<PendingActionApproval[]>([]);

	// Per-id deny-reason state. Surfaced inline next to the action's
	// args so the user types their reason where they're looking, rather
	// than navigating to a detail view. Empty string is fine — the
	// runtime forwards "" verbatim and the agent reads "user denied"
	// without commentary.
	let denyReasons = $state<Record<string, string>>({});

	// Per-id pending-decide flag so we can disable the buttons during
	// the in-flight POST. Without this, double-click during latency
	// produces a "404 already resolved" race the user can't interpret.
	let deciding = $state<Record<string, boolean>>({});

	let loading = $state(true);
	let error = $state('');

	// Action-level approvals stream live over SSE — no polling. The
	// stream emits a `snapshot` on connect, then `pending` / `resolved`
	// events as the queue mutates. The browser's EventSource handles
	// reconnect.
	function applyPending(item: PendingActionApproval) {
		// De-dupe by id so a snapshot replay (after reconnect) doesn't
		// double the card.
		if (actionApprovals.some((a) => a.id === item.id)) return;
		actionApprovals = [...actionApprovals, item];
	}

	function applyResolved(id: string) {
		actionApprovals = actionApprovals.filter((a) => a.id !== id);
		delete denyReasons[id];
	}

	async function decide(id: string, approved: boolean) {
		// Snapshot the reason at click-time so a concurrent typing
		// keystroke doesn't change what gets sent. Empty for approve.
		const reason = approved ? '' : (denyReasons[id] ?? '');
		deciding[id] = true;
		try {
			await decideActionApproval(id, approved, reason);
			// Optimistically drop the entry; the SSE `resolved` event
			// will arrive shortly and reconcile if the server view
			// differs.
			applyResolved(id);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			deciding[id] = false;
		}
	}

	function formatRequestedAt(ts: string): string {
		try {
			return new Date(ts).toLocaleString();
		} catch {
			return ts;
		}
	}

	function formatArgs(args: Record<string, unknown> | undefined): string {
		if (!args || Object.keys(args).length === 0) return '(no args)';
		try {
			return JSON.stringify(args, null, 2);
		} catch {
			return String(args);
		}
	}

	onMount(() => {
		const closeStream = watchActionApprovals({
			onSnapshot: (items) => {
				actionApprovals = items;
				loading = false;
				error = '';
			},
			onPending: applyPending,
			onResolved: (r) => applyResolved(r.id),
			onError: (e) => {
				error = e instanceof Error ? e.message : String(e);
			}
		});
		return () => {
			closeStream();
		};
	});
</script>

<svelte:head>
	<title>Approvals — Aileron</title>
</svelte:head>

<h1 class="mb-4 text-xl font-semibold">Approvals</h1>

{#if loading}
	<p class="text-muted-foreground">Connecting to the approval stream…</p>
{:else if error}
	<p class="text-destructive">{error}</p>
{:else if actionApprovals.length === 0}
	<p class="text-muted-foreground">
		No pending approvals. The agent's blocked tool calls (if any) will appear here.
	</p>
{:else}
	<section data-testid="action-approvals-section">
		<p class="mb-3 text-sm text-muted-foreground">
			The agent is blocked on these tool calls until you approve or deny.
		</p>
		<div class="flex flex-col gap-3">
			{#each actionApprovals as approval (approval.id)}
				<Card.Root data-testid="action-approval-card" data-approval-id={approval.id}>
					<Card.Header>
						<div class="flex items-center justify-between">
							<div>
								<span class="font-semibold">{approval.action_name}</span>
								{#if approval.connector_fqn}
									<span class="ml-3 text-sm text-muted-foreground">
										via {approval.connector_fqn}
									</span>
								{/if}
							</div>
							<span class="text-xs text-muted-foreground">
								{formatRequestedAt(approval.requested_at)}
							</span>
						</div>
					</Card.Header>
					<Card.Content class="flex flex-col gap-3">
						<pre
							class="overflow-x-auto rounded bg-muted p-3 text-xs"
							data-testid="approval-args">{formatArgs(approval.args)}</pre>
						<div class="flex items-center gap-2">
							<input
								type="text"
								placeholder="Optional reason (deny only)"
								class="flex-1 rounded border border-input bg-background px-2 py-1 text-sm"
								bind:value={denyReasons[approval.id]}
								disabled={deciding[approval.id]}
								data-testid="deny-reason-input"
							/>
							<Button
								variant="default"
								disabled={deciding[approval.id]}
								data-testid="approve-button"
								onclick={() => decide(approval.id, true)}
							>
								Approve
							</Button>
							<Button
								variant="destructive"
								disabled={deciding[approval.id]}
								data-testid="deny-button"
								onclick={() => decide(approval.id, false)}
							>
								Deny
							</Button>
						</div>
						{#if approval.session_id}
							<div class="text-xs text-muted-foreground">
								Session: {approval.session_id}
							</div>
						{/if}
					</Card.Content>
				</Card.Root>
			{/each}
		</div>
	</section>
{/if}
