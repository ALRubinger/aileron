<script lang="ts">
	import HighlightCard from '$lib/components/ui/highlight-card.svelte';
	import { Button } from '$lib/components/ui/button/index.js';
	import Plane from '@lucide/svelte/icons/plane';
	import ArrowRight from '@lucide/svelte/icons/arrow-right';
</script>

<!--
  Large CTA card pointing readers from the homepage intro into the
  Getting Started flow. Static markup; no client hydration needed.
-->
<HighlightCard variant="muted" class="not-prose">
	<div class="flex flex-col items-start gap-6 md:flex-row md:items-center md:justify-between">
		<div class="flex items-start gap-4">
			<div class="rounded-lg bg-primary/10 p-3 text-primary">
				<Plane class="size-7" />
			</div>
			<div>
				<h3 class="text-2xl font-semibold leading-tight">Liftoff in 5 minutes</h3>
				<p class="mt-1 text-base text-muted-foreground">
					Install Aileron, equip Gmail and Calendar actions, and launch Claude Code with everything wired in.
				</p>
			</div>
		</div>
		<Button
			href="/getting-started/"
			class="h-12 self-stretch px-6 text-base text-primary-foreground no-underline transition-transform duration-150 hover:scale-95 md:self-auto"
		>
			Start
			<ArrowRight class="ml-1" />
		</Button>
	</div>
</HighlightCard>
