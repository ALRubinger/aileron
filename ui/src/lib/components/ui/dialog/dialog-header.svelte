<script lang="ts">
	import { cn } from "$lib/utils.js";
	import type { HTMLAttributes } from "svelte/elements";

	let {
		class: className,
		children,
		...restProps
	}: HTMLAttributes<HTMLDivElement> & { children?: import("svelte").Snippet } = $props();
</script>

<div
	data-slot="dialog-header"
	class={cn("flex flex-col gap-1.5 text-center sm:text-left", className)}
	{...restProps}
>
	{#if children}
		{@render children()}
	{/if}
</div>
