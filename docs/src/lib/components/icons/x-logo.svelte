<script lang="ts">
  /**
   * X (formerly Twitter) brand mark. Not shipped by @lucide/svelte (its
   * `x.svelte` is the generic close icon); inlined here following the
   * same `size` + `class` API as Lucide icons and mcp-logo.svelte.
   *
   * Path: Simple Icons (CC0). https://simpleicons.org/?q=x
   */
  let {
    size = 20,
    class: className = '',
  }: {
    size?: number;
    class?: string;
  } = $props();
</script>

<svg
  xmlns="http://www.w3.org/2000/svg"
  width={size}
  height={size}
  viewBox="0 0 24 24"
  fill="currentColor"
  aria-hidden="true"
  class={className}
>
  <path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z" />
</svg>
