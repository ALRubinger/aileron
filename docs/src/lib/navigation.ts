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
      { label: 'Liftoff in 5 Minutes', href: '/getting-started/' },
      { label: 'v4', href: '/meet-aileron/v4/' }
    ]
  },
  {
    label: 'Concepts',
    children: [
      { label: 'Deterministic Agentic Execution', href: '/concepts/deterministic-agentic-execution/' },
      { label: 'Actions', href: '/concepts/actions/' },
      { label: 'Connectors', href: '/concepts/connectors/' },
      { label: 'The Vault', href: '/concepts/the-vault/' },
      { label: 'Proof of Control', href: '/concepts/proof-of-control/' },
      { label: 'The Keyring', href: '/concepts/the-keyring/' }
    ]
  },
  {
    label: 'Actions',
    children: [
      { label: 'Google', href: '/actions/google/' },
      { label: 'iMessage via BlueBubbles', href: '/actions/imessage-via-bluebubbles/' },
      { label: 'Slack', href: '/actions/slack/' }
    ]
  },
  {
    label: 'Guides',
    children: [
      { label: 'Discovering Connectors', href: '/guides/discovering-connectors/' },
      { label: 'Installing a Connector', href: '/guides/installing-a-connector/' },
      { label: 'Installing an Action', href: '/guides/installing-an-action/' },
      { label: 'Installing a Skill', href: '/guides/installing-a-skill/' },
      { label: 'Authoring a Connector', href: '/guides/authoring-a-connector/' },
      { label: 'Setting up BlueBubbles for Aileron', href: '/guides/setting-up-bluebubbles/' },
      { label: 'Authoring an Action', href: '/guides/authoring-an-action/' },
      { label: 'Authoring an Action Suite', href: '/guides/authoring-an-action-suite/' },
      { label: 'Publishing a Connector', href: '/guides/publishing-a-connector/' },
      { label: 'Publishing to the Hub', href: '/guides/publishing-to-the-hub/' },
      { label: 'Observability', href: '/guides/observability/' },
      { label: 'Binding Descriptors', href: '/guides/binding-descriptors/' },
      { label: 'Add Your Own CLI Tool to the Sandbox', href: '/guides/customizing-the-sandbox/' }
    ]
  },
  {
    label: 'Development',
    defaultOpen: false,
    children: [
      { label: 'Overview', href: '/development/' },
      { label: 'Repo Layout', href: '/development/repo-layout/' },
      { label: 'Binary Architecture', href: '/development/binary-architecture/' },
      { label: 'Building from Source', href: '/development/building-from-source/' },
      { label: 'Running Tests', href: '/development/running-tests/' },
      { label: 'Sandbox Composition', href: '/development/sandbox-composition/' },
      { label: 'Sandbox Agent Images', href: '/development/sandbox-agent-images/' },
      { label: 'Sandbox Connector Specs', href: '/development/sandbox-connector-specs/' },
      { label: 'Flight Plan Manifest Spec', href: '/development/flight-plan-manifest-spec/' },
      { label: 'Submitting Changes', href: '/development/submitting-changes/' }
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
      { label: 'ADR-0011: Local Credential Vault', href: '/adr/0011-local-credential-vault/' },
      { label: 'ADR-0012: Local Daemon Architecture', href: '/adr/0012-local-daemon-architecture/' },
      { label: 'ADR-0013: Connector Hub and Trust Distribution', href: '/adr/0013-connector-hub-and-trust-distribution/' },
      { label: 'ADR-0014: Spawn Sandbox Technology', href: '/adr/0014-spawn-sandbox-technology/' },
      { label: 'ADR-0015: Launch Audit Scope', href: '/adr/0015-launch-audit-scope/' },
      { label: 'ADR-0016: Approval-Time Preview Fetch', href: '/adr/0016-approval-preview/' },
      { label: 'ADR-0017: Sandbox Composition', href: '/adr/0017-sandbox-composition/' }
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
