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
	const contentClass = cn(
		"cn-accordion-content overflow-hidden pb-4 text-sm",
		"[&_pre]:my-2"
	);
</script>

<!--
  BuildPrereqs is the dev-toolchain accordion shown under the "Build
  from source" method tab in InstallTabs. Single Svelte component
  owning the Accordion primitives directly so bits-ui's accordion
  context stays intact (the same constraint that Tabs has).
-->
<AccordionPrimitive.Root type="multiple" class="cn-build-prereqs my-2 w-full">
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
</AccordionPrimitive.Root>
