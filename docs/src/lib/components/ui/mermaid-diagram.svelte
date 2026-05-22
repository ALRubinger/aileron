<script lang="ts">
  import { onMount } from 'svelte';

  /**
   * Renders a Mermaid diagram from a graph definition string.
   * Loads mermaid from CDN on mount so no build-time dependency is required.
   * Used by docs pages that include architecture or sequence diagrams.
   */
  let {
    graph,
    class: className = ''
  }: {
    graph: string;
    class?: string;
  } = $props();

  let container: HTMLDivElement;
  let error = $state<string | null>(null);

  onMount(async () => {
    try {
      // Dynamic import keeps mermaid out of the SSR bundle (it requires DOM globals).
      const mod = await import('mermaid');
      const mermaid = mod.default;
      mermaid.initialize({
        startOnLoad: false,
        theme: 'base',
        themeVariables: {
          primaryColor: '#f1f5f9',
          primaryTextColor: '#0f172a',
          primaryBorderColor: '#475569',
          lineColor: '#475569',
          secondaryColor: '#dbeafe',
          tertiaryColor: '#fef3c7',
          fontFamily: 'ui-sans-serif, system-ui, sans-serif',
          fontSize: '14px'
        },
        flowchart: { curve: 'basis', padding: 20 },
        sequence: { actorMargin: 60, boxMargin: 10, messageMargin: 35 }
      });
      const id = `mermaid-${Math.random().toString(36).slice(2)}`;
      const { svg } = await mermaid.render(id, graph);
      if (container) container.innerHTML = svg;
    } catch (e) {
      error = (e as Error)?.message ?? 'Failed to render diagram';
    }
  });
</script>

<div
  bind:this={container}
  class="mermaid-diagram not-prose flex justify-center overflow-x-auto rounded-lg border border-slate-200 bg-white p-4 {className}"
>
  {#if error}
    <pre class="text-sm text-red-600">Diagram failed to render: {error}</pre>
  {/if}
</div>
