<script lang="ts">
	import {
		decideActionApproval,
		watchActionApprovals,
		type PendingActionApproval
	} from '$lib/api';
	import { onMount } from 'svelte';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import * as Collapsible from '$lib/components/ui/collapsible';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import ApprovalFieldsBlock from '$lib/components/ApprovalFieldsBlock.svelte';

	// Action-level approvals (#418): runtime-blocking yes/no for actions
	// whose manifest declared `[approval] required = true`. Each entry
	// represents one held-open `RunAction` HTTP response.
	//
	// #428 broadens the surface to four kinds:
	//   - `action`        — the original manifest-driven gate
	//   - `comms_send`    — `aileron-mcp`'s send_message tool
	//   - `comms_draft`   — draft_reply with editable body
	//   - `http_request`  — http_request with credential transparency
	let actionApprovals = $state<PendingActionApproval[]>([]);

	// Per-id deny-reason state. Surfaced inline next to the action's
	// args so the user types their reason where they're looking, rather
	// than navigating to a detail view. Empty string is fine — the
	// runtime forwards "" verbatim and the agent reads "user denied"
	// without commentary.
	let denyReasons = $state<Record<string, string>>({});

	// Per-id editable draft body for `comms_draft` kind. Initialised
	// from the entry's args.draft_body so the textarea starts with the
	// agent's proposed reply; the user types over it before approving.
	let draftBodies = $state<Record<string, string>>({});

	// Per-id pending-decide flag so we can disable the buttons during
	// the in-flight POST. Without this, double-click during latency
	// produces a "404 already resolved" race the user can't interpret.
	let deciding = $state<Record<string, boolean>>({});

	let loading = $state(true);
	let error = $state('');

	// `?focus=<approval-id>` deep-link target, set by `aileron open
	// approval <id>` (cmd/aileron/open.go) and by the terminal notifier.
	// When the matching card renders, it's scrolled into view and gets a
	// distinct ring so the operator can pick it out from sibling cards.
	let focusedId = $state('');
	let scrolledToFocused = false;

	let focusedMissing = $derived(
		!loading &&
			!error &&
			focusedId !== '' &&
			!actionApprovals.some((a) => a.id === focusedId)
	);

	$effect(() => {
		if (!focusedId || scrolledToFocused) return;
		if (!actionApprovals.some((a) => a.id === focusedId)) return;
		const el = document.querySelector(
			`[data-approval-id="${CSS.escape(focusedId)}"]`
		);
		if (!el) return;
		el.scrollIntoView({ behavior: 'smooth', block: 'center' });
		scrolledToFocused = true;
	});

	// Action-level approvals stream live over SSE — no polling. The
	// stream emits a `snapshot` on connect, then `pending` / `resolved`
	// events as the queue mutates. The browser's EventSource handles
	// reconnect.
	function applyPending(item: PendingActionApproval) {
		// De-dupe by id so a snapshot replay (after reconnect) doesn't
		// double the card.
		if (actionApprovals.some((a) => a.id === item.id)) return;
		actionApprovals = [...actionApprovals, item];
		if (item.kind === 'comms_draft') {
			const initial = (item.args?.draft_body as string | undefined) ?? '';
			draftBodies[item.id] = initial;
		}
	}

	function applyResolved(id: string) {
		actionApprovals = actionApprovals.filter((a) => a.id !== id);
		delete denyReasons[id];
		delete draftBodies[id];
	}

	async function decide(
		id: string,
		approved: boolean,
		opts?: { editedPayload?: Record<string, unknown> }
	) {
		// Snapshot the reason at click-time so a concurrent typing
		// keystroke doesn't change what gets sent. Empty for approve.
		const reason = approved ? '' : (denyReasons[id] ?? '');
		deciding[id] = true;
		try {
			await decideActionApproval(id, approved, reason, opts?.editedPayload);
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

	async function approveDraft(id: string, originalDraft: string) {
		const edited = (draftBodies[id] ?? '').trim();
		// Only attach edited_payload when the user actually changed
		// something — keeps the wire payload minimal in the common
		// "approve as-is" case.
		const payload =
			edited && edited !== originalDraft.trim() ? { body: edited } : undefined;
		await decide(id, true, { editedPayload: payload });
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

	function entryKind(a: PendingActionApproval): PendingActionApproval['kind'] {
		// Older payloads from a daemon predating #428 won't carry
		// `kind`; treat them as the historic action gate so the page
		// still renders something useful instead of a blank card.
		return a.kind ?? 'action';
	}

	function asString(v: unknown): string {
		return typeof v === 'string' ? v : '';
	}

	onMount(() => {
		const params = new URLSearchParams(window.location.search);
		focusedId = params.get('focus') ?? '';

		const closeStream = watchActionApprovals({
			onSnapshot: (items) => {
				actionApprovals = items;
				for (const item of items) {
					if (item.kind === 'comms_draft') {
						draftBodies[item.id] = (item.args?.draft_body as string | undefined) ?? '';
					}
				}
				loading = false;
				error = '';
			},
			onPending: applyPending,
			onResolved: (r) => applyResolved(r.id),
			onError: (e) => {
				// Surface the failure instead of leaving the page on the
				// "Connecting…" placeholder forever. `loading` is also
				// cleared here so the error branch renders — the SSE
				// stream may never deliver a snapshot if the browser gave
				// up (readyState === CLOSED).
				error = e instanceof Error ? e.message : String(e);
				loading = false;
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

<Tooltip.Provider>
{#if loading}
	<p class="text-muted-foreground">Connecting to the approval stream…</p>
{:else if error}
	<p class="text-destructive">{error}</p>
{:else if actionApprovals.length === 0}
	{#if focusedMissing}
		<p
			class="mb-3 rounded border border-border bg-muted/40 p-2 text-sm text-muted-foreground"
			data-testid="focused-missing-banner"
		>
			Approval <code class="rounded bg-muted px-1">{focusedId}</code> is no longer pending.
		</p>
	{/if}
	<p class="text-muted-foreground">
		No pending approvals. The agent's blocked tool calls (if any) will appear here.
	</p>
{:else}
	<section data-testid="action-approvals-section">
		<p class="mb-3 text-sm text-muted-foreground">
			The agent is blocked on these tool calls until you approve or deny.
		</p>
		{#if focusedMissing}
			<p
				class="mb-3 rounded border border-border bg-muted/40 p-2 text-sm text-muted-foreground"
				data-testid="focused-missing-banner"
			>
				Approval <code class="rounded bg-muted px-1">{focusedId}</code> is no longer pending.
			</p>
		{/if}
		<div class="flex flex-col gap-3">
			{#each actionApprovals as approval (approval.id)}
				{@const kind = entryKind(approval)}
				{@const isFocused = approval.id === focusedId}
				<Card.Root
					data-testid="action-approval-card"
					data-approval-id={approval.id}
					data-approval-kind={kind}
					data-focused={isFocused ? 'true' : 'false'}
					class={isFocused ? 'ring-2 ring-primary' : ''}
				>
					<Card.Header>
						<div class="flex items-center justify-between">
							<div>
								<span class="font-semibold">{approval.action_name}</span>
								{#if approval.connector_fqn}
									<span class="ml-3 text-sm text-muted-foreground">
										via {approval.connector_fqn}
									</span>
								{/if}
								<span class="ml-3 rounded bg-muted px-2 py-0.5 text-xs font-medium uppercase tracking-wide text-muted-foreground">
									{kind.replace('_', ' ')}
								</span>
							</div>
							<span class="text-xs text-muted-foreground">
								{formatRequestedAt(approval.requested_at)}
							</span>
						</div>
					</Card.Header>
					<Card.Content class="flex flex-col gap-3">
						{#if approval.preview}
							{@const preview = approval.preview}
							<div
								class="rounded border border-border bg-muted/40 p-3 text-sm"
								data-testid="approval-preview"
							>
								<div class="mb-1 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
									Preview
								</div>
								{#if preview.unavailable}
									<div
										class="text-xs italic text-muted-foreground"
										data-testid="approval-preview-unavailable"
									>
										{preview.unavailable}
									</div>
								{:else}
									<ApprovalFieldsBlock
										fields={preview.fields ?? []}
										testIdPrefix="approval-preview"
									/>
								{/if}
							</div>
						{/if}

						{#if kind === 'comms_send'}
							<div data-testid="comms-send-summary" class="space-y-1 text-sm">
								<div>
									<span class="font-medium">Send to:</span>
									{asString(approval.args?.service)} {asString(approval.args?.channel)}
								</div>
								<pre
									class="overflow-x-auto rounded bg-muted p-3 text-xs"
									data-testid="comms-send-body">{asString(approval.args?.body)}</pre>
							</div>
						{:else if kind === 'comms_draft'}
							<div data-testid="comms-draft-summary" class="space-y-2 text-sm">
								<div>
									<span class="font-medium">Reply to:</span>
									{asString(approval.args?.original_author)} in
									{asString(approval.args?.service)} {asString(approval.args?.channel)}
								</div>
								{#if asString(approval.args?.original_body)}
									<div class="rounded bg-muted/60 p-2 text-xs italic">
										“{asString(approval.args?.original_body)}”
									</div>
								{/if}
								<label class="flex flex-col gap-1 text-xs">
									<span class="font-medium">Draft (editable):</span>
									<textarea
										class="min-h-24 w-full rounded border border-input bg-background p-2 text-sm"
										data-testid="comms-draft-body-input"
										bind:value={draftBodies[approval.id]}
										disabled={deciding[approval.id]}
									></textarea>
								</label>
							</div>
						{:else if kind === 'http_request'}
							<div data-testid="http-request-summary" class="space-y-1 text-sm">
								<div>
									<span class="font-medium">{asString(approval.args?.method)}</span>
									<span class="ml-2 break-all">{asString(approval.args?.url)}</span>
								</div>
								{#if asString(approval.args?.secret_name)}
									<div class="text-xs text-muted-foreground">
										Will inject credential
										<code class="rounded bg-muted px-1">{asString(approval.args?.secret_name)}</code>
										as a Bearer token.
									</div>
								{:else}
									<div class="text-xs text-muted-foreground">
										No matching api_key binding — the request will go out unauthenticated.
									</div>
								{/if}
								{#if asString(approval.args?.body)}
									<pre
										class="overflow-x-auto rounded bg-muted p-3 text-xs"
										data-testid="http-request-body">{asString(approval.args?.body)}</pre>
								{/if}
							</div>
						{:else}
							{#if approval.input_fields && approval.input_fields.length > 0}
								<div
									class="rounded border border-border bg-muted/40 p-3 text-sm"
									data-testid="approval-input-fields-block"
								>
									<ApprovalFieldsBlock
										fields={approval.input_fields}
										testIdPrefix="approval-input"
									/>
								</div>
							{/if}
							<Collapsible.Root data-testid="approval-args-accordion">
								<Tooltip.Root>
									<Tooltip.Trigger>
										{#snippet child({ props })}
											<Collapsible.Trigger
												{...props}
												class="group flex items-center gap-1 text-xs font-medium text-muted-foreground hover:text-foreground"
												data-testid="approval-args-trigger"
											>
												<span
													class="inline-block transition-transform group-data-[state=open]:rotate-90"
													aria-hidden="true">▸</span
												>
												Raw inputs (JSON)
											</Collapsible.Trigger>
										{/snippet}
									</Tooltip.Trigger>
									<Tooltip.Content>
										The exact JSON the agent passed. Kept available for
										debugging; the labeled fields above are the
										authoritative summary.
									</Tooltip.Content>
								</Tooltip.Root>
								<Collapsible.Content>
									<pre
										class="mt-1 overflow-x-auto rounded bg-muted p-3 text-xs"
										data-testid="approval-args">{formatArgs(approval.args)}</pre>
								</Collapsible.Content>
							</Collapsible.Root>
						{/if}

						<div class="flex flex-wrap items-center gap-2">
							<input
								type="text"
								placeholder="Optional reason (deny only)"
								class="flex-1 rounded border border-input bg-background px-2 py-1 text-sm"
								bind:value={denyReasons[approval.id]}
								disabled={deciding[approval.id]}
								data-testid="deny-reason-input"
							/>
							{#if kind === 'comms_draft'}
								<Button
									variant="default"
									disabled={deciding[approval.id]}
									data-testid="approve-button"
									onclick={() =>
										approveDraft(approval.id, asString(approval.args?.draft_body))}
								>
									Approve & Send
								</Button>
								<Button
									variant="destructive"
									disabled={deciding[approval.id]}
									data-testid="deny-button"
									onclick={() => decide(approval.id, false)}
								>
									Discard
								</Button>
							{:else}
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
							{/if}
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
</Tooltip.Provider>
