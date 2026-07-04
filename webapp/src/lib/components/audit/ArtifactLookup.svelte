<script lang="ts">
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';

	// Artifact lookup: paste a `sha256:<hex>` hash, or drop a file to hash
	// its bytes client-side (crypto.subtle) into the same `sha256:<hex>`
	// form. Either path calls back with the resolved hash so the page can
	// render that artifact's provenance graph.
	let { onlookup }: { onlookup: (hash: string) => void } = $props();

	let hashInput = $state('');
	let hashing = $state(false);
	let dropError = $state('');

	function normalize(raw: string): string {
		const trimmed = raw.trim();
		if (!trimmed) return '';
		// Accept a bare hex digest too, prefixing the algorithm.
		if (/^[0-9a-fA-F]{64}$/.test(trimmed)) return `sha256:${trimmed.toLowerCase()}`;
		return trimmed;
	}

	function submitHash() {
		const h = normalize(hashInput);
		if (h) onlookup(h);
	}

	async function hashFile(file: File): Promise<string> {
		const buf = await file.arrayBuffer();
		const digest = await crypto.subtle.digest('SHA-256', buf);
		const hex = Array.from(new Uint8Array(digest))
			.map((b) => b.toString(16).padStart(2, '0'))
			.join('');
		return `sha256:${hex}`;
	}

	async function onDrop(e: DragEvent) {
		e.preventDefault();
		dropError = '';
		const file = e.dataTransfer?.files?.[0];
		if (!file) return;
		hashing = true;
		try {
			const h = await hashFile(file);
			hashInput = h;
			onlookup(h);
		} catch (err) {
			dropError = err instanceof Error ? err.message : String(err);
		} finally {
			hashing = false;
		}
	}
</script>

<section class="rounded-lg border border-border bg-card p-4" data-testid="artifact-lookup">
	<h2 class="mb-2 text-sm font-semibold">Look up an artifact by content hash</h2>
	<div class="flex flex-wrap items-center gap-2">
		<Input
			type="text"
			placeholder="sha256:…"
			bind:value={hashInput}
			data-testid="lookup-hash-input"
			class="max-w-md"
			onkeydown={(e: KeyboardEvent) => {
				if (e.key === 'Enter') submitHash();
			}}
		/>
		<Button data-testid="lookup-hash-submit" onclick={submitHash} disabled={!hashInput.trim()}>
			Look up
		</Button>
	</div>

	<div
		role="button"
		tabindex="0"
		data-testid="lookup-dropzone"
		class="mt-3 flex h-20 items-center justify-center rounded border border-dashed border-border text-sm text-muted-foreground"
		ondragover={(e) => e.preventDefault()}
		ondrop={onDrop}
	>
		{#if hashing}
			Hashing file…
		{:else}
			Drop a file here to hash it locally and look up its provenance
		{/if}
	</div>
	{#if dropError}
		<p class="mt-1 text-xs text-destructive" data-testid="lookup-drop-error">{dropError}</p>
	{/if}
</section>
