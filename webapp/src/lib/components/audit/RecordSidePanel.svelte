<script lang="ts">
	import * as Dialog from '$lib/components/ui/dialog';
	import * as Collapsible from '$lib/components/ui/collapsible';
	import { Badge } from '$lib/components/ui/badge';
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

	// Full field rows for an artifact/step node, read from the backing
	// event's flat payload. Ordered for readability.
	const rows = $derived.by((): Row[] => {
		if (!node?.event) return [];
		const e = node.event;
		const out: Row[] = [];
		const add = (label: string, v: string | number | undefined, mono = false) => {
			if (v !== undefined && v !== '') out.push({ label, value: String(v), mono });
		};
		add('Name', p.outputName(e));
		add('MIME', p.outputMime(e));
		add('Content hash', p.outputContentHash(e), true);
		add('Bytes', p.outputBytes(e));
		add('Path', p.outputPath(e), true);
		add('Step id', p.stepId(e));
		add('Step kind', p.stepKind(e));
		add('Transform', p.stepTransform(e));
		add('Command', p.stepCommand(e), true);
		add('Plan / skill', p.planSkill(e));
		add('Plan hash', p.planContentHash(e), true);
		add('Signed by', p.planSignedBy(e), true);
		add('Signature status', p.planSignatureStatus(e));
		add('Actor', p.actorIdentityLabel(e));
		add('Credential binding', p.actorCredentialBinding(e));
		add('Connector version', p.actorConnectorVersion(e));
		add('Connector hash', p.actorConnectorHash(e), true);
		add('Consent', p.consentDecision(e));
		add('Invocation id', p.invocationId(e), true);
		return out;
	});

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
