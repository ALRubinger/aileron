<script lang="ts">
  import { onMount } from 'svelte';
  import { slide } from 'svelte/transition';
  import { externalLinks, isSection, type NavItem } from '../lib/navigation';
  import Plane from '@lucide/svelte/icons/plane';
  import Rocket from '@lucide/svelte/icons/rocket';
  import Server from '@lucide/svelte/icons/server';
  import Wrench from '@lucide/svelte/icons/wrench';
  import GitFork from '@lucide/svelte/icons/git-fork';
  import Braces from '@lucide/svelte/icons/braces';
  import Cloud from '@lucide/svelte/icons/cloud';
  import Compass from '@lucide/svelte/icons/compass';
  import Layers from '@lucide/svelte/icons/layers';
  import BookOpen from '@lucide/svelte/icons/book-open';
  import Terminal from '@lucide/svelte/icons/terminal';

  let { currentPath = '', navigation = [] as NavItem[] }: { currentPath?: string; navigation?: NavItem[] } = $props();
  let mobileOpen = $state(false);
  let mounted = $state(false);

  // Track which sections the user has manually toggled (label → open/closed)
  // Persisted in localStorage so state survives Astro page navigations
  const STORAGE_KEY = 'sidebar-toggles';

  function loadToggles(): Record<string, boolean> {
    try {
      const stored = localStorage.getItem(STORAGE_KEY);
      return stored ? JSON.parse(stored) : {};
    } catch { return {}; }
  }

  function saveToggles(toggles: Record<string, boolean>) {
    try { localStorage.setItem(STORAGE_KEY, JSON.stringify(toggles)); } catch {}
  }

  let manualToggles: Record<string, boolean> = $state(loadToggles());

  onMount(() => {
    mounted = true;
  });

  function isSectionOpen(item: import('../lib/navigation').NavSection): boolean {
    // Manual toggle always wins
    const manual = manualToggles[item.label];
    if (manual !== undefined) return manual;
    // Default: open if it contains the active page, or if defaultOpen
    return sectionContainsActive(item) || item.defaultOpen !== false;
  }

  function toggleSection(label: string, currentlyOpen: boolean) {
    manualToggles[label] = !currentlyOpen;
    saveToggles(manualToggles);
  }

  // Map top-level labels to icons
  const iconMap: Record<string, typeof Plane> = {
    'Meet Aileron': Plane,
    'Architecture': Compass,
    'Getting Started': Rocket,
    'Operations': Server,
    'Deployment': Cloud,
    'Development': Wrench,
    'Architecture Decisions': GitFork,
    'Concepts': Layers,
    'Guides': BookOpen,
    'CLI Reference': Terminal,
    'API Reference': Braces
  };

  function isActive(href: string): boolean {
    return currentPath === href || currentPath === href + '/';
  }

  function sectionContainsActive(item: NavItem): boolean {
    if (!isSection(item)) return isActive(item.href);
    return item.children.some(child => sectionContainsActive(child));
  }
</script>

<!-- Mobile toggle button -->
<button
  class="fixed top-4 left-3 z-50 p-2 rounded-md bg-background border border-border opacity-100 visible translate-x-0 transition-[opacity,visibility,translate] duration-200 lg:opacity-0 lg:invisible lg:-translate-x-12"
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
  class="fixed top-16 left-0 z-40 h-[calc(100vh-4rem)] w-80 shrink-0 border-r border-border bg-background overflow-y-auto transition-transform duration-200
    {mobileOpen ? 'translate-x-0' : '-translate-x-full'} lg:translate-x-0 lg:sticky lg:top-16 lg:h-[calc(100vh-4rem)]"
>
  <div class="p-4">
    <nav class="flex flex-col gap-4 {mounted ? 'visible' : 'invisible'}">
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
  {@const open = isSectionOpen(item)}
  {@const Icon = iconMap[item.label]}
  <div class="group {depth > 0 ? 'ml-2' : ''} rounded border border-border {active ? 'bg-accent' : 'bg-muted'}">
    <button
      class="w-full flex items-center justify-between py-1.5 px-2 rounded text-sm font-medium select-none cursor-pointer
        {active ? 'text-accent-foreground' : 'text-foreground'} hover:bg-accent/50"
      onclick={() => toggleSection(item.label, open)}
    >
      <span class="flex items-center gap-2">
        {#if Icon}
          <Icon size={16} class="shrink-0 transition-transform duration-150 {active ? 'scale-125' : 'group-hover:scale-125'}" />
        {/if}
        {item.label}
      </span>
      <svg class="w-4 h-4 transition-transform duration-200 {open ? 'rotate-90' : ''}" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <polyline points="9 18 15 12 9 6" />
      </svg>
    </button>
    {#if open}
      <div class="ml-2 pl-2" transition:slide={{ duration: mounted ? 150 : 0 }}>
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
    {/if}
  </div>
{/snippet}
