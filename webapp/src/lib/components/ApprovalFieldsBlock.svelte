<script lang="ts">
	import type { ActionApprovalPreviewField } from '$lib/api';

	// Shared labeled-fields renderer used by approval cards. The
	// approval page renders this twice: once for the action manifest's
	// `[approval.preview]` directive (external state fetched by the
	// runtime, per ADR-0016) and once for the manifest's `[[inputs]]`
	// declarations (the agent's call-time args projected through the
	// per-input `label` / `multiline` keys from the ADR-0003 amendment).
	//
	// Both surfaces share the same shape — inline `key: value` rows for
	// short fields and scrollable blockquotes for multiline fields — so
	// factoring the renderer here keeps the two flows from drifting and
	// gives webapp tests a single render path to assert against.
	//
	// `whitespace-pre-wrap` on the multiline blockquote is the load-
	// bearing rule: it draws embedded `\n` characters from the value as
	// real newlines, which is what the user expects when they read an
	// email body or commit message in the approval surface.
	//
	// The blockquote opens at a small preview height (`h-32`) with a
	// `min-h-16` floor and `resize-y` so the user can drag the corner to
	// expand it and read the whole value before authorizing an
	// irreversible send. `overflow-y-auto` keeps a scrollbar for the
	// remainder until they resize. There is deliberately no `max-h`: a
	// cap would stop the drag short of revealing a long body, which is
	// the exact review gap this affordance closes.

	type Props = {
		fields: ActionApprovalPreviewField[];
		/** Test-id stem the rendered DOM nodes pick up so the page-
		 *  level test suite can target preview-vs-input renders
		 *  unambiguously (`approval-preview-fields` vs
		 *  `approval-input-fields`). */
		testIdPrefix: string;
	};

	let { fields, testIdPrefix }: Props = $props();

	let inlineFields = $derived(fields.filter((f) => !f.multiline));
	let blockFields = $derived(fields.filter((f) => f.multiline));
</script>

{#if inlineFields.length > 0}
	<dl
		class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm"
		data-testid="{testIdPrefix}-fields"
	>
		{#each inlineFields as field (field.label)}
			<dt class="font-medium text-muted-foreground">{field.label}:</dt>
			<dd
				class="break-words"
				data-testid="{testIdPrefix}-field"
				data-field-label={field.label}
				data-field-missing={field.missing ? 'true' : 'false'}
			>
				{#if field.missing}
					<span class="italic text-muted-foreground">n/a</span>
				{:else}
					{field.value ?? ''}
				{/if}
			</dd>
		{/each}
	</dl>
{/if}
{#each blockFields as field (field.label)}
	<div
		class="mt-2"
		data-testid="{testIdPrefix}-multiline"
		data-field-label={field.label}
		data-field-missing={field.missing ? 'true' : 'false'}
	>
		<div class="mb-1 text-xs font-medium text-muted-foreground">
			{field.label}
		</div>
		{#if field.missing}
			<div class="text-sm italic text-muted-foreground">n/a</div>
		{:else}
			<blockquote
				class="h-32 min-h-16 resize-y overflow-y-auto whitespace-pre-wrap break-words rounded border-l-2 border-border bg-background/60 px-3 py-2 text-sm"
			>{field.value ?? ''}</blockquote>
		{/if}
	</div>
{/each}
