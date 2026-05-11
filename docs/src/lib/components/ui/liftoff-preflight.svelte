<script lang="ts">
	import { Accordion as AccordionPrimitive } from "bits-ui";
	import ChevronDown from "@lucide/svelte/icons/chevron-down";
	import { cn } from "$lib/utils.js";

	const triggerClass = cn(
		"flex w-full items-center justify-between py-3 text-left text-sm font-medium",
		"hover:underline focus-visible:outline-1 focus-visible:outline-ring",
		"[&[data-state=open]>svg]:rotate-180"
	);
	const itemClass = "border-b border-border";
	// `cn-accordion-content` is the hook for the open/close height
	// animation defined in global.css (driven by bits-ui's
	// `--bits-accordion-content-height` CSS variable on data-state).
	const contentClass = cn(
		"cn-accordion-content overflow-hidden pb-4 text-sm",
		"[&_pre]:my-2"
	);
</script>

<!--
  LiftoffPreflight owns the "Liftoff in 5 Minutes" preflight checklist
  as a single Svelte island. Splitting Accordion primitives across MDX
  references would sever bits-ui's context (see the comment on
  platform-tabs.svelte). Page-specific by design — the prose lives
  here so the data and presentation stay together for content that
  only ships on this one page.
-->
<AccordionPrimitive.Root type="multiple" class="cn-preflight my-6 w-full">

	<AccordionPrimitive.Item value="claude-code" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>Claude Code</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content class={contentClass}>
			<p>
				Install and configure
				<a href="https://docs.claude.com/en/docs/claude-code/setup">Claude Code</a> with an
				Anthropic API key in <code>ANTHROPIC_API_KEY</code>. Aileron's launcher routes Claude
				Code's LLM calls through Aileron locally. It does not supply or replace your API key.
			</p>
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>

	<AccordionPrimitive.Item value="google" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>A Google account</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content class={contentClass}>
			<p>Gmail and Calendar must be enabled. The OAuth dance opens in your default browser.</p>
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>
</AccordionPrimitive.Root>
