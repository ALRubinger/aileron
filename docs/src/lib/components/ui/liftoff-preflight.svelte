<script lang="ts">
	import { Accordion as AccordionPrimitive } from "bits-ui";
	import ChevronDown from "@lucide/svelte/icons/chevron-down";
	import { cn } from "$lib/utils.js";

	const triggerClass = cn(
		"flex w-full items-center justify-between py-3 text-left text-sm font-medium",
		"cursor-pointer hover:bg-muted/60 transition-colors duration-150 px-3 -mx-3 rounded-md focus-visible:outline-1 focus-visible:outline-ring",
		"[&[data-state=open]>svg]:rotate-180"
	);
	const itemClass = "border-b border-border";
	// `cn-accordion-content` is the hook for the open/close height
	// animation defined in global.css (driven by bits-ui's
	// `--bits-accordion-content-height` CSS variable on data-state).
	const contentClass = cn(
		"cn-accordion-content overflow-hidden text-sm",
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

	<AccordionPrimitive.Item value="agent-credential" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>An agent credential</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content forceMount class={contentClass}>
			<p>
				<code>aileron launch</code> runs the agent inside the container, so you do not install the
				agent CLI on your host. You only need a credential the launcher can seed into the sandbox.
				Two agents are supported.
			</p>
			<p>
				Claude needs either a Claude Pro/Max subscription or an Anthropic API key. Subscription mode
				signs in with a Claude account via OAuth login and is the default. API-key mode reads an
				Anthropic API key from <code>ANTHROPIC_API_KEY</code>. Pick the mode at launch with
				<code>--claude-auth=subscription|api-key</code>, or answer the first-run prompt where Enter
				selects subscription.
			</p>
			<p>
				Codex needs either a Codex/OpenAI subscription or an OpenAI API key. Both agents seed the
				credential the same way. First launch runs an in-container login, or a host-side acquirer
				stores the credential in the vault before the container starts. The vault then renders it
				into every later launch silently. See
				<a href="/development/sandbox-agent-auth/">Sandbox Agent Auth</a> for the full
				mode-selection and seeding detail.
			</p>
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>

	<AccordionPrimitive.Item value="docker" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>Docker</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content forceMount class={contentClass}>
			<p>
				Docker installed and the daemon running.
				<a href="https://www.docker.com/products/docker-desktop/">Docker Desktop</a> is the easy
				path on macOS and Windows. <code>aileron launch</code> runs the agent inside a container by
				default, so the daemon must be reachable before you launch.
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
		<AccordionPrimitive.Content forceMount class={contentClass}>
			<p>Gmail and Calendar must be enabled. The OAuth dance opens in your default browser.</p>
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>
</AccordionPrimitive.Root>
