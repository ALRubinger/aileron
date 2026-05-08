<script lang="ts">
  import type { Snippet } from 'svelte';
  import * as Card from '$lib/components/ui/card/index.js';
  import { cn } from '$lib/utils.js';

  /**
   * Aileron-custom HighlightCard — a prominent panel for in-page
   * summaries / orientation callouts. Wraps shadcn-svelte's Card with
   * the standardized "primary" (left-border accent) and "muted"
   * (subtle background) styling reused across the privacy policy,
   * terms of use, and Getting Started pages.
   *
   * Default variant matches the privacy/terms "short version" boxes;
   * the muted variant reads as a softer, secondary callout that sits
   * alongside a primary one (e.g. an "expectations" panel below an
   * intro panel).
   */
  type Variant = 'primary' | 'muted';

  let {
    title,
    variant = 'primary',
    class: className,
    children,
  }: {
    title?: string;
    variant?: Variant;
    class?: string;
    children: Snippet;
  } = $props();

  const variantClass: Record<Variant, string> = {
    primary: 'border-l-4 border-l-primary',
    muted: 'bg-muted/50',
  };
</script>

<Card.Root class={cn('my-8 text-base', variantClass[variant], className)}>
  {#if title}
    <Card.Header>
      <Card.Title class="text-lg">{title}</Card.Title>
    </Card.Header>
  {/if}
  <Card.Content>
    {@render children()}
  </Card.Content>
</Card.Root>
