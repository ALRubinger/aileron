<script lang="ts">
	import * as Dialog from '$lib/components/ui/dialog';
	import * as Collapsible from '$lib/components/ui/collapsible';
	import { Badge } from '$lib/components/ui/badge';
	import type { AuditEvent } from '$lib/api';
	import type { ProvenanceNode } from '$lib/audit/provenance';
	import * as p from '$lib/audit/payload';

	// Side panel opened when a node is clicked. Shows the full record
	// fields as labeled rows (never a raw full-JSON dump in the default
	// view). Large literal input values render collapsed via a collapsible
	// as `name · mime · size · [view]` and expand on demand only.
	let {
		node = $bindable(null)
	}: { node: ProvenanceNode | null } = $props();

	// The dialog is open exactly when a node is selected; closing it clears
	// the selection so the parent's binding stays consistent.
	let open = $state(false);
	$effect(() => {
		open = node !== null;
	});
	function onOpenChange(next: boolean) {
		open = next;
		if (!next) node = null;
	}

	// A literal value counts as "large" past this length; it renders
	// collapsed with a descriptor and a [view] toggle.
	const LARGE_LITERAL = 120;

	type Row = { label: string; value: string; mono?: boolean };

	function rowsFrom(
		e: AuditEvent,
		specs: Array<[label: string, v: string | number | undefined, mono?: boolean]>
	): Row[] {
		const out: Row[] = [];
		for (const [label, v, mono = false] of specs) {
			if (v !== undefined && v !== '') out.push({ label, value: String(v), mono });
		}
		return out;
	}

	// Artifact/step record fields, minus the chain-of-custody group below.
	const rows = $derived.by((): Row[] => {
		if (!node?.event) return [];
		const e = node.event;
		return rowsFrom(e, [
			['Name', p.outputName(e)],
			['MIME', p.outputMime(e)],
			['Content hash', p.outputContentHash(e), true],
			['Bytes', p.outputBytes(e)],
			['Path', p.outputPath(e), true],
			['Step id', p.stepId(e)],
			['Step kind', p.stepKind(e)],
			['Transform', p.stepTransform(e)],
			['Command', p.stepCommand(e), true],
			['Plan / skill', p.planSkill(e)],
			['Plan hash', p.planContentHash(e), true],
			['Invocation id', p.invocationId(e), true]
		]);
	});

	// The explicit chain-of-custody group: who signed the plan, whether the
	// signature verified, the acting identity, its credential binding, and the
	// consent decision. Grouped so the trust story reads as one unit.
	const custodyRows = $derived.by((): Row[] => {
		if (!node?.event) return [];
		const e = node.event;
		return rowsFrom(e, [
			['Signed by', p.planSignedBy(e), true],
			['Signature status', p.planSignatureStatus(e)],
			['Actor', p.actorIdentityLabel(e)],
			['Credential binding', p.actorCredentialBinding(e)],
			['Connector version', p.actorConnectorVersion(e)],
			['Connector hash', p.actorConnectorHash(e), true],
			['Consent', p.consentDecision(e)]
		]);
	});

	const custodyVerified = $derived(node?.event ? p.signatureVerified(node.event) : false);
	const showCustody = $derived(!!node?.dangling || custodyRows.length > 0);

	const inputs = $derived(node?.event ? p.stepInputs(node.event) : []);
</script>

<Dialog.Root bind:open {onOpenChange}>
	<Dialog.Content class="sm:max-w-lg">
		{#if node}
			<Dialog.Header>
				<Dialog.Title data-testid="side-panel-title">{node.title}</Dialog.Title>
				<Dialog.Description>{node.kind} record</Dialog.Description>
			</Dialog.Header>

			<div class="max-h-[60vh] overflow-y-auto" data-testid="side-panel-body">
				{#if node.kind === 'literal'}
					{@const source = node.literal?.source ?? ''}
					<div data-testid="literal-descriptor" class="text-sm">
						<div class="flex flex-wrap items-center gap-2">
							<span class="font-medium">{node.literal?.binding}</span>
							{#if source.length > LARGE_LITERAL}
								<Badge variant="outline">{source.length} chars</Badge>
							{/if}
						</div>
						{#if source.length > LARGE_LITERAL}
							<Collapsible.Root class="mt-2">
								<Collapsible.Trigger
									data-testid="literal-view-toggle"
									class="text-xs text-primary underline underline-offset-2"
								>
									view
								</Collapsible.Trigger>
								<Collapsible.Content>
									<pre
										data-testid="literal-full-value"
										class="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-all rounded bg-muted p-2 text-xs">{source}</pre>
								</Collapsible.Content>
							</Collapsible.Root>
						{:else if source}
							<p class="mt-1 break-all font-mono text-xs text-muted-foreground">{source}</p>
						{/if}
					</div>
				{:else}
					<dl class="grid grid-cols-1 gap-y-2 text-sm">
						{#each rows as row (row.label)}
							<div class="flex flex-col">
								<dt class="text-xs font-medium text-muted-foreground">{row.label}</dt>
								<dd class="break-all {row.mono ? 'font-mono text-xs' : ''}">{row.value}</dd>
							</div>
						{/each}
					</dl>

					{#if showCustody}
						<div class="mt-4 rounded-md border border-border p-3" data-testid="custody-section">
							<div class="mb-2 flex items-center justify-between gap-2">
								<h3 class="text-xs font-semibold text-muted-foreground">Chain of custody</h3>
								{#if custodyRows.length > 0}
									<Badge
										variant={custodyVerified ? 'default' : 'destructive'}
										data-testid="custody-indicator"
									>
										{custodyVerified ? 'Verified' : 'Unverified'}
									</Badge>
								{/if}
							</div>
							{#if node.dangling}
								<p class="mb-2 text-xs font-medium text-destructive" data-testid="custody-gap-note">
									Producer not recorded — provenance gap
								</p>
							{/if}
							{#if custodyRows.length > 0}
								<dl class="grid grid-cols-1 gap-y-2 text-sm">
									{#each custodyRows as row (row.label)}
										<div class="flex flex-col">
											<dt class="text-xs font-medium text-muted-foreground">{row.label}</dt>
											<dd class="break-all {row.mono ? 'font-mono text-xs' : ''}">{row.value}</dd>
										</div>
									{/each}
								</dl>
							{/if}
						</div>
					{/if}

					{#if inputs.length > 0}
						<div class="mt-4">
							<h3 class="mb-1 text-xs font-semibold text-muted-foreground">Inputs</h3>
							<ul class="flex flex-col gap-1 text-xs">
								{#each inputs as input, i (input.binding + i)}
									<li class="flex flex-wrap items-center gap-2" data-testid="side-panel-input">
										<span class="font-medium">{input.binding}</span>
										{#if input.source}<span class="font-mono text-muted-foreground"
												>{input.source}</span
											>{/if}
										{#if input.content_hash}<Badge variant="secondary"
												>{p.shortHash(input.content_hash)}</Badge
											>{:else}<Badge variant="outline">literal</Badge>{/if}
									</li>
								{/each}
							</ul>
						</div>
					{/if}
				{/if}
			</div>
		{/if}
	</Dialog.Content>
</Dialog.Root>
