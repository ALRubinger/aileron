<script lang="ts" module>
	import { Tabs as TabsPrimitive } from "bits-ui";
	import { tabsListVariants } from "./tabs-list.svelte";
	import { cn } from "$lib/utils.js";
	import CodeBlock from "../code-block.svelte";

	export type PlatformTabItem = {
		value: string;
		label: string;
		lang?: string;
		code: string;
		note?: string;
	};
</script>

<script lang="ts">
	let {
		items,
		defaultValue,
		class: className,
	}: {
		items: PlatformTabItem[];
		defaultValue?: string;
		class?: string;
	} = $props();

	let value = $state(defaultValue ?? items[0]?.value ?? "");
</script>

<!--
  PlatformTabs is a single Svelte island that owns Tabs root, list, all triggers, and all
  content panels. Astro+MDX hydrates one island per Svelte component reference; splitting
  Tabs.Root and Tabs.Trigger across MDX breaks bits-ui's context. Keeping the entire tabs
  surface in one component preserves context and matches the cross-platform install pattern
  this site needs (one bullet per prerequisite, three OS tabs, one code block each).
-->
<TabsPrimitive.Root
	bind:value
	data-slot="tabs"
	class={cn("cn-tabs platform-tabs group/tabs flex flex-col my-3", className)}
>
	<TabsPrimitive.List
		data-slot="tabs-list"
		data-variant="default"
		class={cn(tabsListVariants({ variant: "default" }), "rounded-md p-1")}
	>
		{#each items as item (item.value)}
			<TabsPrimitive.Trigger
				value={item.value}
				data-slot="tabs-trigger"
				class={cn(
					"cn-tabs-trigger relative inline-flex items-center justify-center whitespace-nowrap rounded-sm px-3 py-1 text-sm transition-all",
					"text-foreground/60 hover:text-foreground",
					"data-active:bg-background data-active:text-foreground data-active:shadow-sm",
					"focus-visible:outline-1 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-50"
				)}
			>
				{item.label}
			</TabsPrimitive.Trigger>
		{/each}
	</TabsPrimitive.List>

	{#each items as item (item.value)}
		<TabsPrimitive.Content
			value={item.value}
			data-slot="tabs-content"
			class={cn("cn-tabs-content flex-1 outline-none mt-2")}
		>
			{#if item.note}
				<p class="text-sm">{item.note}</p>
			{/if}
			<CodeBlock code={item.code} lang={item.lang} />
		</TabsPrimitive.Content>
	{/each}
</TabsPrimitive.Root>
