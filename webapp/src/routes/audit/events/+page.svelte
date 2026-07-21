<script lang="ts">
	import { onMount } from 'svelte';
	import * as Card from '$lib/components/ui/card';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import { listAudit, type AuditEvent } from '$lib/api';
	import * as p from '$lib/audit/payload';

	// `/audit/events` — the general audit-events feed (Option B, #2162). A
	// sibling to the artifact-scoped provenance view at `/audit`: where that
	// view walks a materialized artifact back to its inputs, this one lists
	// every audit event newest-first, rendering the same
	// timestamp/audit-id/event-type/summary line the `aileron audit` CLI
	// prints. The summary column reuses the CLI's `auditPayloadSummary` port
	// so the two surfaces read identically for the same event.

	let events = $state<AuditEvent[]>([]);
	let loading = $state(true);
	let error = $state('');

	// Guards against a slow earlier load() (e.g. a double-clicked refresh)
	// resolving after a newer one and overwriting fresher data or clearing
	// the newer request's loading state. Only the latest invocation applies
	// its result — mirrors the invocation-id staleness guard on the
	// provenance view.
	let loadVersion = 0;

	async function load() {
		const version = ++loadVersion;
		loading = true;
		error = '';
		try {
			const result = await listAudit();
			if (version === loadVersion) events = result;
		} catch (e) {
			if (version === loadVersion) error = e instanceof Error ? e.message : String(e);
		} finally {
			if (version === loadVersion) loading = false;
		}
	}

	onMount(() => {
		void load();
	});
</script>

<svelte:head>
	<title>Audit events — Aileron</title>
</svelte:head>

<div class="mb-4 flex flex-wrap items-center justify-between gap-2">
	<h1 class="text-3xl font-extrabold tracking-tight">Audit events</h1>
	<a
		href="/audit"
		data-testid="to-provenance"
		class="text-sm text-primary no-underline hover:underline">Provenance &amp; artifacts →</a
	>
</div>

<p class="mb-4 text-sm text-muted-foreground">
	Every audit event the running daemon has recorded, newest first. To walk a
	materialized artifact back to the launch, steps, and inputs that produced it,
	use the <a href="/audit" class="text-primary hover:underline">provenance view</a>.
</p>

{#if loading}
	<p class="text-muted-foreground">Loading audit events…</p>
{:else if error}
	<p class="text-destructive" data-testid="events-error">{error}</p>
{:else if events.length === 0}
	<p class="text-muted-foreground" data-testid="events-empty">
		No audit events recorded yet. Actions, approvals, and Flight Plan launches
		appear here as the daemon records them.
	</p>
{:else}
	<div class="flex flex-col gap-3" data-testid="events-list">
		{#each events as event (event.audit_id)}
			{@const summary = p.auditPayloadSummary(event)}
			<Card.Root data-testid="event-row">
				<Card.Header>
					<div class="flex items-center justify-between gap-2">
						<span class="font-mono text-xs text-muted-foreground">{event.timestamp}</span>
						<Badge variant="outline" data-testid="event-type">{event.event_type}</Badge>
					</div>
				</Card.Header>
				<Card.Content>
					<p class="font-mono text-xs text-muted-foreground" data-testid="event-audit-id">
						{event.audit_id}
					</p>
					{#if summary}
						<p class="mt-1 text-sm" data-testid="event-summary">{summary}</p>
					{/if}
				</Card.Content>
			</Card.Root>
		{/each}
	</div>
{/if}

<div class="mt-4">
	<Button variant="ghost" size="sm" data-testid="events-refresh" disabled={loading} onclick={load}
		>Refresh</Button
	>
</div>
