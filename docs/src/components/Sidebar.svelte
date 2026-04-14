<script lang="ts">
  import { navigation, externalLinks, isSection, type NavItem } from '../lib/navigation';
  import Plane from '@lucide/svelte/icons/plane';
  import Rocket from '@lucide/svelte/icons/rocket';
  import Server from '@lucide/svelte/icons/server';
  import Wrench from '@lucide/svelte/icons/wrench';
  import GitFork from '@lucide/svelte/icons/git-fork';
  import Braces from '@lucide/svelte/icons/braces';
  import Cloud from '@lucide/svelte/icons/cloud';

  let { currentPath = '' }: { currentPath?: string } = $props();
  let mobileOpen = $state(false);

  // Map top-level labels to icons
  const iconMap: Record<string, typeof Plane> = {
    'Meet Aileron': Plane,
    'Getting Started': Rocket,
    'Operations': Server,
    'Deployment': Cloud,
    'Development': Wrench,
    'Architecture Decisions': GitFork,
    'API Reference': Braces
  };

  function isActive(href: string): boolean {
    return currentPath === href || currentPath === href + '/';
  }

  function sectionContainsActive(item: NavItem): boolean {
    if (!isSection(item)) return isActive(item.href);
    if (item.href && isActive(item.href)) return true;
    return item.children.some(child => sectionContainsActive(child));
  }
</script>

<!-- Mobile toggle button -->
<button
  class="lg:hidden fixed top-3 left-3 z-50 p-2 rounded-md bg-background border border-border"
  onclick={() => mobileOpen = !mobileOpen}
  aria-label="Toggle navigation"
>
  <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    {#if mobileOpen}
      <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
    {:else}
      <line x1="3" y1="12" x2="21" y2="12" /><line x1="3" y1="6" x2="21" y2="6" /><line x1="3" y1="18" x2="21" y2="18" />
    {/if}
  </svg>
</button>

<!-- Backdrop for mobile -->
{#if mobileOpen}
  <div
    class="lg:hidden fixed inset-0 z-30 bg-black/50"
    onclick={() => mobileOpen = false}
    onkeydown={(e) => { if (e.key === 'Escape') mobileOpen = false; }}
    role="button"
    tabindex="-1"
    aria-label="Close navigation"
  ></div>
{/if}

<!-- Sidebar -->
<aside
  class="fixed top-0 left-0 z-40 h-screen w-80 shrink-0 border-r border-border bg-background overflow-y-auto transition-transform duration-200
    {mobileOpen ? 'translate-x-0' : '-translate-x-full'} lg:translate-x-0 lg:sticky lg:top-0"
>
  <div class="p-4">
    <a href="/" class="font-bold text-lg block mb-6 text-foreground no-underline hover:text-foreground">
      Aileron Docs
    </a>
    <nav>
      {#each navigation as item}
        {#if isSection(item)}
          {@render section(item, 0)}
        {:else}
          {@const active = isActive(item.href)}
          {@const Icon = iconMap[item.label]}
          <a
            href={item.href}
            class="group flex items-center gap-2 py-1.5 px-2 rounded text-sm no-underline
              {active ? 'bg-accent text-accent-foreground font-medium' : 'text-muted-foreground hover:text-accent-foreground hover:bg-accent/70'}"
            onclick={() => mobileOpen = false}
          >
            {#if Icon}
              <Icon size={16} class="shrink-0 transition-transform duration-150 {active ? 'scale-125' : 'group-hover:scale-125'}" />
            {/if}
            {item.label}
          </a>
        {/if}
      {/each}

      <!-- External links -->
      <div class="mt-6 pt-4 border-t border-border">
        {#each externalLinks as link}
          {@const active = currentPath === link.href || currentPath === link.href + '/'}
          {@const Icon = iconMap[link.label]}
          <a
            href={link.href}
            class="group flex items-center gap-2 py-1.5 px-2 rounded text-sm no-underline
              {active ? 'bg-accent text-accent-foreground font-medium' : 'text-muted-foreground hover:text-accent-foreground hover:bg-accent/70'}"
            onclick={() => mobileOpen = false}
          >
            {#if Icon}
              <Icon size={16} class="shrink-0 transition-transform duration-150 {active ? 'scale-125' : 'group-hover:scale-125'}" />
            {/if}
            {link.label}
          </a>
        {/each}
      </div>
    </nav>
  </div>
</aside>

{#snippet section(item: import('../lib/navigation').NavSection, depth: number)}
  {@const active = sectionContainsActive(item)}
  {@const open = active || item.defaultOpen !== false}
  {@const Icon = iconMap[item.label]}
  <details open={open} class="group/section {depth > 0 ? 'ml-2' : ''} rounded border {active ? 'bg-accent/30 border-border' : 'border-transparent'}">
    <summary
      class="group/summary flex items-center justify-between py-1.5 px-2 rounded text-sm font-medium select-none
        {item.href && isActive(item.href) ? 'bg-accent text-accent-foreground' : active ? 'text-accent-foreground' : 'text-foreground'} hover:bg-accent/50 list-none [&::-webkit-details-marker]:hidden
        {item.href ? '' : 'cursor-pointer'}"
      onclick={item.href ? (e: MouseEvent) => { e.preventDefault(); } : undefined}
    >
      {#if item.href}
        <a
          href={item.href}
          class="flex-1 flex items-center gap-2 no-underline {active && isActive(item.href) ? 'text-accent-foreground' : 'text-inherit'}"
          onclick={(e) => { e.stopPropagation(); mobileOpen = false; }}
        >
          {#if Icon}
            <Icon size={16} class="shrink-0 transition-transform duration-150 {active ? 'scale-125' : 'group-hover/section:scale-125'}" />
          {/if}
          {item.label}
        </a>
      {:else}
        <span class="flex items-center gap-2">
          {#if Icon}
            <Icon size={16} class="shrink-0 transition-transform duration-150 {active ? 'scale-125' : 'group-hover/section:scale-125'}" />
          {/if}
          {item.label}
        </span>
      {/if}
      <button
        class="p-0.5 rounded hover:bg-accent/70 cursor-pointer"
        onclick={(e) => {
          e.stopPropagation();
          e.preventDefault();
          const details = (e.currentTarget as HTMLElement).closest('details');
          if (details) details.open = !details.open;
        }}
        aria-label="Toggle section"
      >
        <svg class="w-4 h-4 transition-transform group-open/section:rotate-90" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <polyline points="9 18 15 12 9 6" />
        </svg>
      </button>
    </summary>
    <div class="ml-2 border-l border-border pl-2">
      {#each item.children as child}
        {#if isSection(child)}
          {@render section(child, depth + 1)}
        {:else}
          <a
            href={child.href}
            class="block py-1 px-2 rounded text-sm no-underline
              {isActive(child.href) ? 'bg-accent text-accent-foreground font-medium' : 'text-muted-foreground hover:text-accent-foreground hover:bg-accent/70'}"
            onclick={() => mobileOpen = false}
          >
            {child.label}
          </a>
        {/if}
      {/each}
    </div>
  </details>
{/snippet}
