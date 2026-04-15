<script lang="ts">
  import Copy from '@lucide/svelte/icons/copy';
  import Eye from '@lucide/svelte/icons/eye';
  import Download from '@lucide/svelte/icons/download';
  import Check from '@lucide/svelte/icons/check';
  import markdownIcon from '../assets/markdown-mark.svg?raw';

  let { slug = '', rawMarkdown = '' }: { slug?: string; rawMarkdown?: string } = $props();
  let copied = $state(false);

  function copyToClipboard() {
    navigator.clipboard.writeText(rawMarkdown);
    copied = true;
    setTimeout(() => copied = false, 2000);
  }

  function downloadMarkdown() {
    const filename = slug ? slug.split('/').pop() + '.md' : 'document.md';
    const blob = new Blob([rawMarkdown], { type: 'text/markdown' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.click();
    URL.revokeObjectURL(url);
  }
</script>

<div class="flex items-center gap-3 rounded border border-border px-3 py-2 text-xs text-muted-foreground">
  <div class="flex flex-col items-center gap-0.5 text-muted-foreground">
    <div class="w-[26px] h-[16px]">{@html markdownIcon}</div>
    <span class="font-medium leading-none">Markdown</span>
  </div>
  <div class="flex flex-col gap-1 flex-1">
    <button
      onclick={copyToClipboard}
      class="inline-flex items-center justify-center gap-1 w-full py-0.5 rounded border border-border hover:bg-accent hover:text-accent-foreground cursor-pointer"
      title="Copy markdown to clipboard"
    >
      {#if copied}
        <Check size={12} />
        Copied
      {:else}
        <Copy size={12} />
        Copy
      {/if}
    </button>
    <a
      href="/raw/{slug}"
      class="inline-flex items-center justify-center gap-1 w-full py-0.5 rounded border border-border hover:bg-accent hover:text-accent-foreground no-underline text-muted-foreground"
      title="View raw markdown"
    >
      <Eye size={12} />
      View
    </a>
    <button
      onclick={downloadMarkdown}
      class="inline-flex items-center justify-center gap-1 w-full py-0.5 rounded border border-border hover:bg-accent hover:text-accent-foreground cursor-pointer"
      title="Download markdown file"
    >
      <Download size={12} />
      Download
    </button>
  </div>
</div>
