<script lang="ts" module>
	import { Tabs as TabsPrimitive } from "bits-ui";
	import { tabsListVariants } from "./tabs-list.svelte";
	import { cn } from "$lib/utils.js";
	import CodeBlock from "../code-block.svelte";

	export type OsInstallVariant = {
		value: string;
		label: string;
		lang?: string;
		code: string;
		note?: string;
	};

	export type OsInstallPlatform = {
		value: string;
		label: string;
		// Either a single code view (lang/code/note here) OR a list of
		// variants rendered as sub-tabs (e.g. Linux's deb/rpm/apk).
		// Mutually exclusive; if both are set, `variants` wins.
		lang?: string;
		code?: string;
		note?: string;
		variants?: OsInstallVariant[];
	};
</script>

<script lang="ts">
	let {
		platforms,
		defaultPlatform,
		class: className,
	}: {
		platforms: OsInstallPlatform[];
		defaultPlatform?: string;
		class?: string;
	} = $props();

	let platformValue = $state(defaultPlatform ?? platforms[0]?.value ?? "");
	let variantValue = $state<Record<string, string>>({});

	function variantFor(platform: OsInstallPlatform): string {
		const cached = variantValue[platform.value];
		if (cached) return cached;
		return platform.variants?.[0]?.value ?? "";
	}

	function setVariant(platformKey: string, value: string) {
		variantValue = { ...variantValue, [platformKey]: value };
	}

	const platformTriggerClass = cn(
		"cn-tabs-trigger relative inline-flex items-center justify-center whitespace-nowrap rounded-sm px-3 py-1 text-sm transition-all",
		"text-foreground/60 hover:text-foreground",
		"data-active:bg-background data-active:text-foreground data-active:shadow-sm",
		"focus-visible:outline-1 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-50"
	);
	const variantTriggerClass = cn(
		"cn-tabs-trigger relative inline-flex items-center justify-center whitespace-nowrap px-2 py-0.5 text-xs transition-colors",
		"text-foreground/50 hover:text-foreground/80",
		"border-b-2 border-transparent",
		"data-active:text-foreground data-active:border-foreground",
		"focus-visible:outline-1 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-50"
	);

	// Notes are author-controlled markup in the page that includes this
	// component. Treat note strings as trusted HTML so anchors and
	// emphasis render — never bind to user input here.
</script>

<!--
  OsInstallTabs is a per-OS install tab strip with an optional inner
  variant strip per platform (e.g. Linux deb/rpm/apk). Two nested
  Tabs.Root layers in one Svelte island so bits-ui's tabs context
  stays intact.
-->
<TabsPrimitive.Root
	bind:value={platformValue}
	data-slot="tabs"
	class={cn("cn-tabs os-install-tabs group/tabs flex flex-col my-3", className)}
>
	<TabsPrimitive.List
		data-slot="tabs-list"
		data-variant="default"
		class={cn(tabsListVariants({ variant: "default" }), "rounded-md p-1")}
	>
		{#each platforms as platform (platform.value)}
			<TabsPrimitive.Trigger
				value={platform.value}
				data-slot="tabs-trigger"
				class={platformTriggerClass}
			>
				{platform.label}
			</TabsPrimitive.Trigger>
		{/each}
	</TabsPrimitive.List>

	{#each platforms as platform (platform.value)}
		<TabsPrimitive.Content
			value={platform.value}
			data-slot="tabs-content"
			class={cn("cn-tabs-content flex-1 outline-none mt-3")}
		>
			{#if platform.note}
				<p class="text-sm">{@html platform.note}</p>
			{/if}

			{#if platform.variants && platform.variants.length > 0}
				<TabsPrimitive.Root
					value={variantFor(platform)}
					onValueChange={(v) => setVariant(platform.value, v)}
					class="cn-tabs flex flex-col mt-2"
				>
					<TabsPrimitive.List
						data-variant="line"
						class={cn(tabsListVariants({ variant: "line" }), "border-b border-border")}
					>
						{#each platform.variants as variant (variant.value)}
							<TabsPrimitive.Trigger value={variant.value} class={variantTriggerClass}>
								{variant.label}
							</TabsPrimitive.Trigger>
						{/each}
					</TabsPrimitive.List>

					{#each platform.variants as variant (variant.value)}
						<TabsPrimitive.Content
							value={variant.value}
							class={cn("cn-tabs-content flex-1 outline-none mt-2")}
						>
							{#if variant.note}
								<p class="text-sm">{@html variant.note}</p>
							{/if}
							<CodeBlock code={variant.code} lang={variant.lang} />
						</TabsPrimitive.Content>
					{/each}
				</TabsPrimitive.Root>
			{:else if platform.code}
				<CodeBlock code={platform.code} lang={platform.lang} />
			{/if}
		</TabsPrimitive.Content>
	{/each}
</TabsPrimitive.Root>
