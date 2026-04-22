/**
 * Sidebar navigation structure.
 *
 * Each entry is either a link (href + label) or a section (label + children).
 * Sections can be nested to any depth. The sidebar component renders this
 * tree with collapsible groups.
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
      { label: 'For Individuals', href: '/for-individuals' },
      { label: 'For Organizations', href: '/for-organizations' },
      { label: 'How Aileron Works', href: '/how-aileron-works' }
    ]
  },
  {
    label: 'Getting Started',
    children: [
      { label: 'Installation', href: '/getting-started/installation' },
      { label: 'Quick Start', href: '/getting-started/quick-start' },
      { label: 'Policy Configuration', href: '/getting-started/policy-configuration' },
      { label: 'Credential Vault', href: '/getting-started/credential-vault' },
      { label: 'Slack Integration', href: '/getting-started/slack-integration' },
      { label: 'Google Integration', href: '/getting-started/google-integration' },
      { label: 'Slack Cloud Integration', href: '/getting-started/slack-cloud-integration' },
      { label: 'Slack App Install (Admin)', href: '/getting-started/slack-app-install' },
      { label: 'Slack Connect (User)', href: '/getting-started/slack-connect' },
      { label: 'GitHub Integration', href: '/getting-started/github-integration' },
      { label: 'Discord Integration', href: '/getting-started/discord-integration' },
      { label: 'Zero-Knowledge Enclave', href: '/getting-started/zero-knowledge-enclave' }
    ]
  },
  {
    label: 'Operations',
    children: [
      { label: 'Supported Agents', href: '/operations/supported-agents' },
      { label: 'Status & Audit', href: '/operations/status-and-audit' },
      { label: 'Running Locally', href: '/operations/running-locally' },
      { label: 'Slack App Configuration', href: '/operations/slack-app-configuration' }
    ]
  },
  {
    label: 'Deployment',
    defaultOpen: false,
    children: [
      { label: 'Cloud Deployment', href: '/deployment/cloud' },
      { label: 'Railway', href: '/deployment/railway' },
      { label: 'TEE Enclave', href: '/deployment/tee-enclave' }
    ]
  },
  {
    label: 'Development',
    defaultOpen: false,
    children: [
      { label: 'Building from Source', href: '/development/building-from-source' },
      { label: 'Testing', href: '/development/testing' },
      { label: 'Project Structure', href: '/development/project-structure' },
      { label: 'Releasing', href: '/development/releasing' }
    ]
  },
  {
    label: 'Architecture Decisions',
    defaultOpen: false,
    children: [] // populated at build time from content collection
  }
];

/**
 * Standalone nav entries rendered below the main tree (e.g. API Reference).
 */
export const externalLinks: NavLink[] = [
  { label: 'API Reference', href: '/api' }
];
