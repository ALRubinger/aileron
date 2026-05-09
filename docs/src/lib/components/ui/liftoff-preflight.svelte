<script lang="ts">
	import { Accordion as AccordionPrimitive } from "bits-ui";
	import ChevronDown from "@lucide/svelte/icons/chevron-down";
	import { cn } from "$lib/utils.js";
	import PlatformTabs, { type PlatformTabItem } from "./tabs/platform-tabs.svelte";

	const goInstall: PlatformTabItem[] = [
		{ value: "macos", label: "macOS", lang: "sh", code: "brew install go" },
		{
			value: "linux",
			label: "Linux",
			lang: "sh",
			note: "Use your distro's package manager, or download the official tarball from go.dev/dl.",
			code: "# Debian / Ubuntu (may lag behind upstream; check version)\nsudo apt-get install golang-go",
		},
		{ value: "windows", label: "Windows", lang: "powershell", code: "winget install GoLang.Go" },
	];

	const taskInstall: PlatformTabItem[] = [
		{ value: "macos", label: "macOS", lang: "sh", code: "brew install go-task" },
		{
			value: "linux",
			label: "Linux",
			lang: "sh",
			code: 'sh -c "$(curl --location https://taskfile.dev/install.sh)" -- -d -b ~/.local/bin',
		},
		{ value: "windows", label: "Windows", lang: "powershell", code: "scoop install task" },
	];

	const nodeInstall: PlatformTabItem[] = [
		{
			value: "macos",
			label: "macOS",
			lang: "sh",
			code: "brew install node\ncorepack enable\ncorepack prepare pnpm@11.0.8 --activate",
		},
		{
			value: "linux",
			label: "Linux",
			lang: "sh",
			note: "Install nvm (https://github.com/nvm-sh/nvm), then run:",
			code: "nvm install 24\ncorepack enable\ncorepack prepare pnpm@11.0.8 --activate",
		},
		{
			value: "windows",
			label: "Windows",
			lang: "powershell",
			code: "winget install OpenJS.NodeJS\ncorepack enable\ncorepack prepare pnpm@11.0.8 --activate",
		},
	];

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
  LiftoffPreflight is a single Svelte island that owns the entire
  "Liftoff in 5 Minutes" preflight checklist: an accordion of bits-ui
  Accordion items, three of which embed a PlatformTabs component for
  per-OS install commands. Splitting Accordion primitives across MDX
  references would sever bits-ui's context (see the comment on
  platform-tabs.svelte). Page-specific by design — the prose lives
  here so the data and presentation stay together for content that
  only ships on this one page.
-->
<AccordionPrimitive.Root type="multiple" class="cn-preflight my-6 w-full">
	<AccordionPrimitive.Item value="go" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>Go 1.25 or newer</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content class={contentClass}>
			<p>
				Aileron's modules require it. <code>go version</code> should report at least
				<code>go1.25.0</code>.
			</p>
			<PlatformTabs items={goInstall} />
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>

	<AccordionPrimitive.Item value="task" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>Task</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content class={contentClass}>
			<p>
				All build commands go through <code>task</code>. See
				<a href="https://taskfile.dev/installation/">taskfile.dev</a> for other install options.
			</p>
			<PlatformTabs items={taskInstall} />
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>

	<AccordionPrimitive.Item value="node" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>Node.js 24 + pnpm 11.0.8</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content class={contentClass}>
			<p>
				The webapp and docs targets build through <code>pnpm</code>, so <code>task build</code> will
				fail without them. The simplest setup is <code>corepack enable</code> after installing Node,
				then <code>corepack prepare pnpm@11.0.8 --activate</code>.
			</p>
			<PlatformTabs items={nodeInstall} />
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>

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
