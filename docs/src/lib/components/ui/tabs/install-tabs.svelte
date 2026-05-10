<script lang="ts" module>
	import { Tabs as TabsPrimitive } from "bits-ui";
	import { tabsListVariants } from "./tabs-list.svelte";
	import { cn } from "$lib/utils.js";
	import CodeBlock from "../code-block.svelte";
	import BuildPrereqs from "../build-prereqs.svelte";

	export type InstallVariant = {
		value: string;
		label: string;
		lang?: string;
		code: string;
		note?: string;
	};

	export type InstallPlatform = {
		value: string;
		label: string;
		// Either a single code view (lang/code/note here) OR a list of
		// variants rendered as a third level of sub-tabs (e.g. Linux's
		// deb/rpm/apk). Mutually exclusive in practice; if both are set,
		// `variants` wins.
		lang?: string;
		code?: string;
		note?: string;
		variants?: InstallVariant[];
	};

	export type InstallMethod = {
		value: string;
		label: string;
		// Either a flat single-block method (e.g. "Build from source"
		// with one cross-platform command) or a per-platform method
		// (e.g. "Install" with macOS/Linux/Windows tabs). If both are
		// set, `platforms` wins.
		lang?: string;
		code?: string;
		note?: string;
		platforms?: InstallPlatform[];
	};
</script>

<script lang="ts">
	let {
		methods,
		defaultMethod,
		defaultPlatform,
		class: className,
	}: {
		methods: InstallMethod[];
		defaultMethod?: string;
		defaultPlatform?: string;
		class?: string;
	} = $props();

	let methodValue = $state(defaultMethod ?? methods[0]?.value ?? "");
	// Per-method active platform/variant state, keyed by method value
	// so switching methods doesn't lose your sub-selection. Initialized
	// lazily inside reactive blocks.
	let platformValue = $state<Record<string, string>>({});
	let variantValue = $state<Record<string, string>>({});

	function platformFor(method: InstallMethod): string {
		const cached = platformValue[method.value];
		if (cached) return cached;
		const fallback = defaultPlatform ?? method.platforms?.[0]?.value ?? "";
		return fallback;
	}

	function variantFor(method: InstallMethod, platform: InstallPlatform): string {
		const key = `${method.value}/${platform.value}`;
		const cached = variantValue[key];
		if (cached) return cached;
		return platform.variants?.[0]?.value ?? "";
	}

	function setPlatform(methodKey: string, value: string) {
		platformValue = { ...platformValue, [methodKey]: value };
	}

	function setVariant(methodKey: string, platformKey: string, value: string) {
		variantValue = { ...variantValue, [`${methodKey}/${platformKey}`]: value };
	}

	// Visual hierarchy: each tab level gets a distinct trigger size +
	// list style so the user reads "Install vs Build from source" as
	// the major decision, "macOS / Linux / Windows" as the platform
	// pivot, and "deb / rpm / apk" as the smaller package-format
	// pivot. Without this differentiation all three rows look like
	// equally-weighted choices.
	const methodTriggerClass = cn(
		"cn-tabs-trigger relative inline-flex items-center justify-center whitespace-nowrap rounded-md px-4 py-1.5 text-base font-medium transition-all",
		"text-foreground/60 hover:text-foreground",
		"data-active:bg-background data-active:text-foreground data-active:shadow-sm",
		"focus-visible:outline-1 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-50"
	);
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
	// component (currently getting-started.mdx). Treat note strings as
	// trusted HTML so anchors and emphasis render — never bind to user
	// input here.
</script>

<!--
  InstallTabs owns three nested Tabs.Root layers (method → platform →
  variant) in a single Svelte island. Splitting any of the layers
  across separate components would sever bits-ui's tabs context (see
  the comment on platform-tabs.svelte for the same constraint at one
  level). Page-specific by design — the install matrix on the
  getting-started page is the only consumer.
-->
<TabsPrimitive.Root
	bind:value={methodValue}
	data-slot="tabs"
	class={cn("cn-tabs install-tabs group/tabs flex flex-col my-3", className)}
>
	<TabsPrimitive.List
		data-slot="tabs-list"
		data-variant="default"
		class={cn(tabsListVariants({ variant: "default" }), "rounded-lg p-1")}
	>
		{#each methods as method (method.value)}
			<TabsPrimitive.Trigger
				value={method.value}
				data-slot="tabs-trigger"
				class={methodTriggerClass}
			>
				{method.label}
			</TabsPrimitive.Trigger>
		{/each}
	</TabsPrimitive.List>

	{#each methods as method (method.value)}
		<TabsPrimitive.Content
			value={method.value}
			data-slot="tabs-content"
			class={cn("cn-tabs-content flex-1 outline-none mt-4")}
		>
			{#if method.note}
				<p class="text-sm">{@html method.note}</p>
			{/if}

			{#if method.value === "source"}
				<BuildPrereqs />
			{/if}

			{#if method.platforms && method.platforms.length > 0}
				<TabsPrimitive.Root
					value={platformFor(method)}
					onValueChange={(v) => setPlatform(method.value, v)}
					class="cn-tabs flex flex-col mt-2"
				>
					<TabsPrimitive.List
						data-variant="default"
						class={cn(tabsListVariants({ variant: "default" }), "rounded-md p-1")}
					>
						{#each method.platforms as platform (platform.value)}
							<TabsPrimitive.Trigger value={platform.value} class={platformTriggerClass}>
								{platform.label}
							</TabsPrimitive.Trigger>
						{/each}
					</TabsPrimitive.List>

					{#each method.platforms as platform (platform.value)}
						<TabsPrimitive.Content
							value={platform.value}
							class={cn("cn-tabs-content flex-1 outline-none mt-3")}
						>
							{#if platform.note}
								<p class="text-sm">{@html platform.note}</p>
							{/if}

							{#if platform.variants && platform.variants.length > 0}
								<TabsPrimitive.Root
									value={variantFor(method, platform)}
									onValueChange={(v) => setVariant(method.value, platform.value, v)}
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
			{:else if method.code}
				<CodeBlock code={method.code} lang={method.lang} />
			{/if}
		</TabsPrimitive.Content>
	{/each}
</TabsPrimitive.Root>
