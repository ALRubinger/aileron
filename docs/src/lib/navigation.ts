/**
 * Sidebar navigation structure.
 *
 * Each entry is either a link (href + label) or a section (label + children).
 * Sections can be nested to any depth. The sidebar component renders this
 * tree with collapsible groups.
 *
 * Currently minimal: the homepage is the only docs page. Historical content
 * lives in `docs/archive/` (off-site) and will be rebuilt page by page during
 * the post-pivot docs rewrite.
 */

import type { Component } from 'svelte';

export type NavLink = {
  label: string;
  href: string;
  icon?: Component;
};

export type NavSection = {
  label: string;
  children: NavItem[];
  defaultOpen?: boolean;
  icon?: Component;
};

export type NavItem = NavLink | NavSection;

export function isSection(item: NavItem): item is NavSection {
  return 'children' in item;
}

// Icons are assigned in Sidebar.svelte to keep this file free of Svelte imports.
// The icon field is populated at render time.

/**
 * The sidebar navigation tree. Update this when adding new docs pages.
 */
export const navigation: NavItem[] = [
  {
    label: 'Meet Aileron',
    children: [
      { label: 'Overview', href: '/' },
      { label: 'Getting Started', href: '/getting-started/' }
    ]
  }
];

/**
 * Standalone nav entries rendered below the main tree (e.g. API Reference).
 */
export const externalLinks: NavLink[] = [
  { label: 'API Reference', href: '/api' }
];
