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
  import Terminal from '@lucide/svelte/icons/terminal';

  let { currentPath = '', navigation = [] as NavItem[] }: { currentPath?: string; navigation?: NavItem[] } = $props();
  let mobileOpen = $state(false);
  let mounted = $state(false);

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

  onMount(() => { mounted = true; });

  function isSectionOpen(item: import('../lib/navigation').NavSection): boolean {
    const manual = manualToggles[item.label];
    if (manual !== undefined) return manual;
    return sectionContainsActive(item) || item.defaultOpen !== false;
  }

  function toggleSection(label: string, currentlyOpen: boolean) {
    manualToggles[label] = !currentlyOpen;
    saveToggles(manualToggles);
  }

  const iconMap: Record<string, typeof Plane> = {
    'Meet Aileron': Plane,
    'Architecture': Compass,
    'Getting Started': Rocket,
    'Operations': Server,
    'Deployment': Cloud,
    'Development': Wrench,
    'Architecture Decisions': GitFork,
    'Concepts': Layers,
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

  <!-- Mobile toggle -->
<button
  class="fixed top-3.5 left-3 z-50 p-2 rounded-md bg-background border border-border opacity-100 visible translate-x-0 transition-[opacity,visibility,translate] duration-200 lg:opacity-0 lg:invisible lg:-translate-x-12"
  onclick={() => mobileOpen = !mobileOpen}
  aria-label="Toggle navigation"
>
  <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    {#if mobileOpen}
      <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
    {:else}
      <line x1="3" y1="12" x2="21" y2="12" /><line x1="3" y1="6" x2="21" y2="6" /><line x1="3" y1="18" x2="21" y2="18" />
    {/if}
  </svg>
</button>

  <!-- Mobile backdrop -->
{#if mobileOpen}
  <div
    class="lg:hidden fixed inset-0 z-30 bg-black/30"
    onclick={() => mobileOpen = false}
    onkeydown={(e) => { if (e.key === 'Escape') mobileOpen = false; }}
    role="button"
    tabindex="-1"
    aria-label="Close navigation"
  ></div>
{/if}

<!-- Sidebar -->
<aside
  class="fixed top-14 left-0 z-40 h-[calc(100vh-3.5rem)] w-72 shrink-0 border-r border-border bg-card chart-graticule overflow-y-auto transition-transform duration-200
    {mobileOpen ? 'translate-x-0' : '-translate-x-full'} lg:translate-x-0 lg:sticky lg:top-14 lg:h-[calc(100vh-3.5rem)]"
>
  <!-- Faint VOR range rings in lower corner — aeronautical texture -->
  <div class="pointer-events-none absolute bottom-0 left-0 overflow-hidden w-48 h-48 opacity-[0.055]" aria-hidden="true">
    <svg viewBox="0 0 200 200" fill="none" xmlns="http://www.w3.org/2000/svg" class="w-full h-full">
      <circle cx="20" cy="180" r="100" stroke="oklch(0.44 0.13 188)" stroke-width="1"/>
      <circle cx="20" cy="180" r="65"  stroke="oklch(0.44 0.13 188)" stroke-width="0.7"/>
      <circle cx="20" cy="180" r="32"  stroke="oklch(0.44 0.13 188)" stroke-width="0.7"/>
      <line x1="20" y1="80"  x2="20"  y2="180" stroke="oklch(0.44 0.13 188)" stroke-width="0.6" stroke-dasharray="3 3"/>
      <line x1="-80" y1="180" x2="120" y2="180" stroke="oklch(0.44 0.13 188)" stroke-width="0.6" stroke-dasharray="3 3"/>
    </svg>
  </div>

  <div class="p-4 relative z-10">
    <nav class="flex flex-col gap-5 {mounted ? 'visible' : 'invisible'}">
      {#each navigation as item}
        {#if isSection(item)}
          {@render section(item, 0)}
        {:else}
          {@const active = isActive(item.href)}
          {@const Icon = iconMap[item.label]}
          <a
            href={item.href}
            class="group flex items-center gap-2 py-1.5 px-2 rounded text-sm no-underline relative
              {active
                ? 'text-primary font-semibold bg-primary/[0.07] pl-5'
                : 'text-muted-foreground hover:text-foreground hover:bg-accent/60'}"
            onclick={() => mobileOpen = false}
          >
            {#if active}
              <!-- Waypoint diamond (aeronautical fix symbol) -->
              <span class="absolute left-1.5 top-1/2 -translate-y-1/2 w-2 h-2 rotate-45 bg-primary" aria-hidden="true"></span>
            {:else if Icon}
              <Icon size={15} class="shrink-0" />
            {/if}
            {item.label}
          </a>
        {/if}
      {/each}

      <!-- External links -->
      <div class="pt-4 border-t border-border">
        {#each externalLinks as link}
          {@const active = currentPath === link.href || currentPath === link.href + '/'}
          {@const Icon = iconMap[link.label]}
          <a
            href={link.href}
            class="group flex items-center gap-2 py-1.5 px-2 rounded text-sm no-underline relative
              {active
                ? 'text-primary font-semibold bg-primary/[0.07] pl-5'
                : 'text-muted-foreground hover:text-foreground hover:bg-accent/60'}"
            onclick={() => mobileOpen = false}
          >
            {#if active}
              <span class="absolute left-1.5 top-1/2 -translate-y-1/2 w-2 h-2 rotate-45 bg-primary" aria-hidden="true"></span>
            {:else if Icon}
              <Icon size={15} class="shrink-0" />
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
  <div class="{depth > 0 ? 'ml-2' : ''} rounded border border-border {active ? 'bg-primary/[0.05]' : 'bg-background/40'}">
    <button
      class="w-full flex items-center justify-between py-1.5 px-2 rounded text-[11px] font-bold uppercase tracking-widest select-none cursor-pointer
        {active ? 'text-primary' : 'text-muted-foreground'} hover:bg-accent/50"
      onclick={() => toggleSection(item.label, open)}
    >
      <span class="flex items-center gap-2">
        {#if Icon}
          <Icon size={13} class="shrink-0 {active ? 'text-primary' : ''}" />
        {/if}
        {item.label}
      </span>
      <svg class="w-3.5 h-3.5 transition-transform duration-200 {open ? 'rotate-90' : ''}" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <polyline points="9 18 15 12 9 6" />
      </svg>
    </button>
    {#if open}
      <div class="ml-2 border-l border-border" transition:slide={{ duration: mounted ? 150 : 0 }}>
        {#each item.children as child}
          {#if isSection(child)}
            {@render section(child, depth + 1)}
          {:else}
            {@const childActive = isActive(child.href)}
            <a
              href={child.href}
              class="flex items-center py-1 px-3 text-[13px] no-underline relative
                {childActive
                  ? 'text-primary font-semibold bg-primary/[0.07] pl-5'
                  : 'text-muted-foreground hover:text-foreground hover:bg-accent/50'}"
              onclick={() => mobileOpen = false}
            >
              {#if childActive}
                <!-- Waypoint diamond -->
                <span class="absolute left-1.5 top-1/2 -translate-y-1/2 w-1.5 h-1.5 rotate-45 bg-primary" aria-hidden="true"></span>
              {/if}
              {child.label}
            </a>
          {/if}
        {/each}
      </div>
    {/if}
  </div>
{/snippet}
  