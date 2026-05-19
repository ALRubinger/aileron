<script lang="ts">
	import Clipboard from "@lucide/svelte/icons/clipboard";
	import Check from "@lucide/svelte/icons/check";

	let {
		code,
		lang,
		html,
	}: {
		code: string;
		lang?: string;
		// Optional pre-rendered Shiki HTML. When set, takes precedence
		// over `code` so callers (typically MDX module scope) can match
		// the themed output of markdown fences. The string must already
		// be a complete <pre>...</pre> from Shiki — we render it raw.
		html?: string;
	} = $props();
</script>

<!--
  Mirrors the structure rehype-copy-button.mjs emits at build time
  (`.code-block-wrapper > button.code-copy-button + pre`). Used inside
  Svelte islands (PlatformTabs, LiftoffPreflight) where MDX-time
  rehype never runs. The click handler in BaseLayout.astro uses event
  delegation so it picks up both static and dynamically-hydrated
  buttons via the same code path.

  When `html` is provided, we render the pre-rendered Shiki output
  instead of a plain <pre><code>{code}</code></pre>. This is how
  Svelte-island code blocks (PlatformTabs etc.) reach visual parity
  with markdown fences, which go through Astro's Shiki integration.
  The copy-button click handler keys on `.code-block-wrapper pre code`
  textContent, which Shiki's output satisfies, so copy still works.
-->
<div class="code-block-wrapper">
	<button type="button" class="code-copy-button" aria-label="Copy code to clipboard">
		<Clipboard size={16} class="icon-clipboard" aria-hidden="true" />
		<Check size={16} class="icon-check" aria-hidden="true" />
	</button>
	{#if html}
		{@html html}
	{:else}
		<pre><code class={lang ? `language-${lang}` : undefined}>{code}</code></pre>
	{/if}
</div>
