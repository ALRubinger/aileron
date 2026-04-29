<script lang="ts">
  import type { Component } from 'svelte';
  import * as Card from '$lib/components/ui/card/index.js';

  /**
   * Aileron-custom Feature block — thin composition over shadcn-svelte's Card.
   * Designed for property triads / scannable proof-point grids.
   *
   * Renders an icon in a small accent square, a title, and a body description.
   * Static — no client:* directive needed.
   */
  let {
    icon: Icon,
    title,
    class: className = ''
  }: {
    icon?: Component;
    title?: string;
    class?: string;
  } = $props();
</script>

<Card.Root size="sm" class={className}>
  <Card.Header>
    {#if Icon}
      <div class="mb-1 flex h-9 w-9 items-center justify-center rounded-md bg-accent text-accent-foreground">
        <Icon size={18} />
      </div>
    {/if}
    {#if title}
      <Card.Title class="leading-tight tracking-tight">{title}</Card.Title>
    {/if}
  </Card.Header>
  <Card.Content class="text-muted-foreground leading-relaxed">
    <slot />
  </Card.Content>
</Card.Root>
