<script lang="ts">
	import { Accordion as AccordionPrimitive } from "bits-ui";
	import ChevronDown from "@lucide/svelte/icons/chevron-down";
	import { cn } from "$lib/utils.js";
	import PlatformTabs, { type PlatformTabItem } from "./tabs/platform-tabs.svelte";

	const golangciLintInstall: PlatformTabItem[] = [
		{ value: "macos", label: "macOS", lang: "sh", code: "brew install golangci-lint" },
		{
			value: "linux",
			label: "Linux",
			lang: "sh",
			code: "curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin",
		},
		{
			value: "windows",
			label: "Windows",
			lang: "powershell",
			code: "scoop install golangci-lint",
		},
	];

	const ghInstall: PlatformTabItem[] = [
		{ value: "macos", label: "macOS", lang: "sh", code: "brew install gh" },
		{
			value: "linux",
			label: "Linux",
			lang: "sh",
			note: "See cli.github.com/manual/installation for other distros.",
			code: "sudo apt-get install gh",
		},
		{ value: "windows", label: "Windows", lang: "powershell", code: "winget install GitHub.cli" },
	];

	const dockerInstall: PlatformTabItem[] = [
		{
			value: "macos",
			label: "macOS",
			lang: "sh",
			note: "Docker Desktop is the simplest path.",
			code: "brew install --cask docker",
		},
		{
			value: "linux",
			label: "Linux",
			lang: "sh",
			note: "Install Docker Engine via your distro; see docs.docker.com/engine/install.",
			code: "curl -fsSL https://get.docker.com | sh",
		},
		{
			value: "windows",
			label: "Windows",
			lang: "powershell",
			code: "winget install Docker.DockerDesktop",
		},
	];

	const triggerClass = cn(
		"flex w-full items-center justify-between py-3 text-left text-sm font-medium",
		"cursor-pointer hover:bg-muted/60 transition-colors duration-150 px-3 -mx-3 rounded-md focus-visible:outline-1 focus-visible:outline-ring",
		"[&[data-state=open]>svg]:rotate-180"
	);
	const itemClass = "border-b border-border";
	const contentClass = cn(
		"cn-accordion-content overflow-hidden text-sm",
		"[&_pre]:my-2"
	);
</script>

<!--
  BuildExtras is the optional-toolchain accordion shown on the
  Building from Source page (golangci-lint, gh, docker). Same
  single-island structure as BuildPrereqs so bits-ui's accordion
  context stays intact.
-->
<AccordionPrimitive.Root type="multiple" class="cn-build-extras my-2 w-full">
	<AccordionPrimitive.Item value="golangci-lint" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>golangci-lint</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content forceMount class={contentClass}>
			<p>Get lint feedback locally ahead of CI.</p>
			<PlatformTabs items={golangciLintInstall} />
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>

	<AccordionPrimitive.Item value="gh" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>gh</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content forceMount class={contentClass}>
			<p>GitHub's CLI, used by the PR workflow.</p>
			<PlatformTabs items={ghInstall} />
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>

	<AccordionPrimitive.Item value="docker" class={itemClass}>
		<AccordionPrimitive.Header>
			<AccordionPrimitive.Trigger class={triggerClass}>
				<span>docker</span>
				<ChevronDown size={16} class="shrink-0 transition-transform" />
			</AccordionPrimitive.Trigger>
		</AccordionPrimitive.Header>
		<AccordionPrimitive.Content forceMount class={contentClass}>
			<p>
				Only required if you're touching the Docker images under <code>deploy/</code>.
			</p>
			<PlatformTabs items={dockerInstall} />
		</AccordionPrimitive.Content>
	</AccordionPrimitive.Item>
</AccordionPrimitive.Root>
