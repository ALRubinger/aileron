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
	data-slot="dialog-footer"
	class={cn("flex flex-col-reverse sm:flex-row sm:justify-end gap-2", className)}
	{...restProps}
>
	{#if children}
		{@render children()}
	{/if}
</div>
