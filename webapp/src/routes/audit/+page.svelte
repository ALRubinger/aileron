<script lang="ts">
	import { onMount } from 'svelte';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import {
		listRecentMaterialized,
		getAuditTrace,
		getAuditByInvocation,
		type AuditEvent
	} from '$lib/api';
	import {
		mapTraceResponse,
		type ProvenanceGraph,
		type ProvenanceNode
	} from '$lib/audit/provenance';
	import * as p from '$lib/audit/payload';
	import GraphView from '$lib/components/audit/GraphView.svelte';
	import ProvenanceHeader from '$lib/components/audit/ProvenanceHeader.svelte';
	import TimelineView from '$lib/components/audit/TimelineView.svelte';
	import RecordSidePanel from '$lib/components/audit/RecordSidePanel.svelte';
	import ArtifactLookup from '$lib/components/audit/ArtifactLookup.svelte';

	// `/audit` provenance view (#1891). Graph-first: the landing lists
	// recent materialized artifacts; selecting one (or deep-linking via a
	// `content_hash` URL query param, the #1894 affordance) resolves that
	// artifact and renders its walk-back DAG. A timeline toggle re-renders
	// the same launch's records in time order.

	let recent = $state<AuditEvent[]>([]);
	let loadingRecent = $state(true);
	let recentError = $state('');

	// The currently-focused artifact graph, when one is selected.
	let root = $state<AuditEvent | null>(null);
	let graph = $state<ProvenanceGraph | null>(null);
	let graphError = $state('');
	let resolving = $state(false);

	let view = $state<'graph' | 'timeline'>('graph');
	let timelineEvents = $state<AuditEvent[]>([]);
	let timelineError = $state('');

	let selectedNode = $state<ProvenanceNode | null>(null);

	async function openArtifact(hash: string) {
		resolving = true;
		graphError = '';
		graph = null;
		root = null;
		view = 'graph';
		// Switching artifacts must not leave a stale timeline behind.
		timelineEvents = [];
		timelineError = '';
		try {
			// One round-trip: the daemon assembles the whole provenance graph
			// (#1913). A 404 (unknown hash) resolves to null — the friendly
			// empty state, not an error (#1894 contract).
			const resp = await getAuditTrace({ contentHash: hash });
			const rootNode = resp?.nodes.find((n) => n.id === resp.root_id);
			if (!resp || !rootNode) {
				graphError = `No artifact found for ${hash}.`;
				return;
			}
			// The root node's embedded event backs the header + timeline id.
			root = rootNode.event ?? null;
			graph = mapTraceResponse(resp);
		} catch (e) {
			graphError = e instanceof Error ? e.message : String(e);
		} finally {
			resolving = false;
		}
	}

	function backToLanding() {
		root = null;
		graph = null;
		graphError = '';
		view = 'graph';
		timelineEvents = [];
		timelineError = '';
	}

	async function showTimeline() {
		view = 'timeline';
		timelineError = '';
		timelineEvents = [];
		if (!root) return;
		const inv = p.invocationId(root);
		if (!inv) {
			timelineError = 'This artifact has no invocation id to correlate a timeline.';
			return;
		}
		try {
			const events = await getAuditByInvocation(inv);
			// Drop the result if the focused artifact changed while this
			// request was in flight, so an older fetch can't overwrite a newer.
			if (root && p.invocationId(root) === inv) {
				timelineEvents = events;
			}
		} catch (e) {
			if (root && p.invocationId(root) === inv) {
				timelineError = e instanceof Error ? e.message : String(e);
			}
		}
	}

	function onNodeSelect(node: ProvenanceNode) {
		selectedNode = node;
	}

	async function loadRecent() {
		loadingRecent = true;
		recentError = '';
		try {
			recent = await listRecentMaterialized();
		} catch (e) {
			recentError = e instanceof Error ? e.message : String(e);
		} finally {
			loadingRecent = false;
		}
	}

	onMount(() => {
		// Deep-link contract for #1894: a `content_hash` query param resolves
		// and renders that artifact's graph directly, skipping the landing.
		const params = new URLSearchParams(window.location.search);
		const deepLink = params.get('content_hash');
		if (deepLink) {
			void openArtifact(deepLink);
		} else {
			void loadRecent();
		}
	});
</script>

<svelte:head>
	<title>Audit — Aileron</title>
</svelte:head>

<h1 class="mb-4 text-3xl font-extrabold tracking-tight">Audit &amp; Provenance</h1>

{#if root && graph}
	<!-- Focused artifact graph / timeline -->
	<div class="mb-4 flex flex-wrap items-center justify-between gap-2">
		<Button variant="ghost" data-testid="back-to-landing" onclick={backToLanding}>
			← All artifacts
		</Button>
		<div class="flex items-center gap-1 rounded-md border border-border p-0.5">
			<Button
				variant={view === 'graph' ? 'default' : 'ghost'}
				size="sm"
				data-testid="view-graph"
				onclick={() => (view = 'graph')}>Graph</Button
			>
			<Button
				variant={view === 'timeline' ? 'default' : 'ghost'}
				size="sm"
				data-testid="view-timeline"
				onclick={showTimeline}>Timeline</Button
			>
		</div>
	</div>

	<ProvenanceHeader {root} {graph} />

	{#if view === 'graph'}
		<GraphView {graph} onselect={onNodeSelect} />
	{:else if timelineError}
		<p class="text-destructive" data-testid="timeline-error">{timelineError}</p>
	{:else}
		<TimelineView events={timelineEvents} />
	{/if}
{:else if resolving}
	<p class="text-muted-foreground">Resolving provenance…</p>
{:else if graphError}
	<p class="mb-4 text-destructive" data-testid="graph-error">{graphError}</p>
	<Button variant="ghost" data-testid="back-to-landing" onclick={backToLanding}
		>← All artifacts</Button
	>
{:else}
	<!-- Landing -->
	<p class="mb-3 text-sm text-muted-foreground">
		Walk any materialized artifact back to the launch, steps, and inputs that
		produced it. Pick a recent artifact below, or look one up by its content
		hash.
	</p>

	<div class="mb-6">
		<ArtifactLookup onlookup={openArtifact} />
	</div>

	<h2 class="mb-2 text-lg font-semibold">Recent artifacts</h2>
	{#if loadingRecent}
		<p class="text-muted-foreground">Loading recent artifacts…</p>
	{:else if recentError}
		<p class="text-destructive" data-testid="recent-error">{recentError}</p>
	{:else if recent.length === 0}
		<p class="text-muted-foreground" data-testid="recent-empty">
			No materialized outputs recorded yet. Run a Flight Plan launch that
			materializes an artifact and it will appear here.
		</p>
	{:else}
		<div class="flex flex-col gap-3" data-testid="recent-list">
			{#each recent as event (event.audit_id)}
				{@const hash = p.outputContentHash(event)}
				<Card.Root data-testid="recent-artifact">
					<button
						type="button"
						class="w-full cursor-pointer text-left"
						onclick={() => hash && openArtifact(hash)}
					>
						<Card.Header>
							<div class="flex items-center justify-between gap-2">
								<span class="font-semibold">{p.outputName(event) ?? '(unnamed)'}</span>
								<span class="text-xs text-muted-foreground">{event.timestamp}</span>
							</div>
						</Card.Header>
						<Card.Content>
							<p class="text-xs text-muted-foreground">
								{[
									p.actorLabel(event) && `by ${p.actorLabel(event)}`,
									p.planSkill(event) && `skill ${p.planSkill(event)}`,
									hash && p.shortHash(hash)
								]
									.filter(Boolean)
									.join(' · ')}
							</p>
						</Card.Content>
					</button>
				</Card.Root>
			{/each}
		</div>
	{/if}
{/if}

<RecordSidePanel bind:node={selectedNode} />
