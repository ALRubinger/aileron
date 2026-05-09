<script lang="ts">
	import Clipboard from "@lucide/svelte/icons/clipboard";
	import Check from "@lucide/svelte/icons/check";

	let {
		code,
		lang,
	}: {
		code: string;
		lang?: string;
	} = $props();
</script>

<!--
  Mirrors the structure rehype-copy-button.mjs emits at build time
  (`.code-block-wrapper > button.code-copy-button + pre`). Used inside
  Svelte islands (PlatformTabs, LiftoffPreflight) where MDX-time
  rehype never runs. The click handler in BaseLayout.astro uses event
  delegation so it picks up both static and dynamically-hydrated
  buttons via the same code path.
-->
<div class="code-block-wrapper">
	<button type="button" class="code-copy-button" aria-label="Copy code to clipboard">
		<Clipboard size={16} class="icon-clipboard" aria-hidden="true" />
		<Check size={16} class="icon-check" aria-hidden="true" />
	</button>
	<pre><code class={lang ? `language-${lang}` : undefined}>{code}</code></pre>
</div>
