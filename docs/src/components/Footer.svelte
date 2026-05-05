<script lang="ts">
  import Lock from '@lucide/svelte/icons/lock';
  import FileText from '@lucide/svelte/icons/file-text';
</script>

<footer class="not-prose mt-16 pt-6 border-t border-border text-sm">
  <nav class="flex flex-col gap-2">
    <a
      href="/privacy"
      class="inline-flex items-center gap-2 text-muted-foreground hover:text-foreground no-underline"
    >
      <Lock size={16} />
      Privacy Policy
    </a>
    <a
      href="/terms"
      class="inline-flex items-center gap-2 text-muted-foreground hover:text-foreground no-underline"
    >
      <FileText size={16} />
      Terms of Use
    </a>
  </nav>
</footer>
