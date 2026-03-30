<script lang="ts">
  import type { Snippet } from 'svelte'

  let { children }: { children: Snippet } = $props()
</script>

<svelte:head>
  <title>Aileron</title>
  <meta name="description" content="Agentic control plane — policy, approvals, and audit for AI agents" />
</svelte:head>

{@render children()}
