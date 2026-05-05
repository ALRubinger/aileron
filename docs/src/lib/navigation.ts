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
  },
  {
    label: 'Concepts',
    children: [
      { label: 'Deterministic Agentic Execution', href: '/concepts/deterministic-agentic-execution/' },
      { label: 'Actions', href: '/concepts/actions/' },
      { label: 'Connectors', href: '/concepts/connectors/' },
      { label: 'The Vault', href: '/concepts/the-vault/' },
      { label: 'Proof of Control', href: '/concepts/proof-of-control/' }
    ]
  },
  {
    label: 'Guides',
    children: [
      { label: 'Authoring a Connector', href: '/guides/authoring-a-connector/' },
      { label: 'Authoring an Action', href: '/guides/authoring-an-action/' },
      { label: 'Publishing a Connector', href: '/guides/publishing-a-connector/' },
      { label: 'Observability', href: '/guides/observability/' }
    ]
  },
  {
    label: 'Architecture Decisions',
    children: [
      { label: 'Overview', href: '/adr/' },
      { label: 'ADR-0001: Manifest Format', href: '/adr/0001-manifest-format/' },
      { label: 'ADR-0002: Connector Model', href: '/adr/0002-connector-model/' },
      { label: 'ADR-0003: Action Model', href: '/adr/0003-action-model/' },
      { label: 'ADR-0004: Dependency Resolution', href: '/adr/0004-dependency-resolution/' },
      { label: 'ADR-0005: Sandbox Choice', href: '/adr/0005-sandbox-choice/' },
      { label: 'ADR-0006: Capability Binding UX', href: '/adr/0006-capability-binding-ux/' },
      { label: 'ADR-0007: Install Consent Flow', href: '/adr/0007-install-consent/' },
      { label: 'ADR-0008: Intent Matching', href: '/adr/0008-intent-matching/' },
      { label: 'ADR-0009: User Channel', href: '/adr/0009-user-channel/' },
      { label: 'ADR-0010: Failure Handling', href: '/adr/0010-failure-handling/' },
      { label: 'ADR-0011: Local Credential Vault', href: '/adr/0011-local-credential-vault/' }
    ]
  }
];

/**
 * Standalone nav entries rendered below the main tree (e.g. API Reference).
 */
export const externalLinks: NavLink[] = [
  { label: 'CLI Reference', href: '/cli/' },
  { label: 'API Reference', href: '/api' }
];
