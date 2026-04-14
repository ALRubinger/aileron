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
  href?: string;
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
    href: '/',
    children: [
      { label: 'For Individuals', href: '/for-individuals' },
      { label: 'For Organizations', href: '/for-organizations' },
      { label: 'How Aileron Works', href: '/how-aileron-works' }
    ]
  },
  {
    label: 'Getting Started',
    href: '/getting-started/installation',
    children: [
      { label: 'Quick Start', href: '/getting-started/quick-start' },
      { label: 'Policy Configuration', href: '/getting-started/policy-configuration' },
      { label: 'Credential Vault', href: '/getting-started/credential-vault' },
      { label: 'Slack Integration', href: '/getting-started/slack-integration' },
      { label: 'Discord Integration', href: '/getting-started/discord-integration' }
    ]
  },
  {
    label: 'Operations',
    children: [
      { label: 'Supported Agents', href: '/operations/supported-agents' },
      { label: 'Status & Audit', href: '/operations/status-and-audit' },
      { label: 'Running Locally', href: '/operations/running-locally' }
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
    children: [
      { label: 'ADR-0001: Hybrid MCP Execution Model', href: '/adr/0001-hybrid-mcp-execution-model' },
      { label: 'ADR-0002: MCP Marketplace Integration', href: '/adr/0002-mcp-marketplace-integration' },
      { label: 'ADR-0003: Marketplace Dashboard UI', href: '/adr/0003-marketplace-dashboard-ui' },
      { label: 'ADR-0004: Enterprise Auth & SSO', href: '/adr/0004-enterprise-auth-sso' },
      { label: 'ADR-0005: GitHub OAuth & Auth SPI', href: '/adr/0005-github-oauth-and-auth-spi-evolution' },
      { label: 'ADR-0006: Stable OAuth Callback Domain', href: '/adr/0006-stable-oauth-callback-domain' },
      { label: 'ADR-0007: Remove OAuth Callback Domain', href: '/adr/0007-remove-stable-oauth-callback-domain' },
      { label: 'ADR-0008: Per-User MCP Server Scoping', href: '/adr/0008-per-user-enterprise-mcp-server-scoping' },
      { label: 'ADR-0009: Deterministic Execution Plane', href: '/adr/0009-deterministic-execution-plane' },
      { label: 'ADR-0010: Zero-Knowledge Vault', href: '/adr/0010-zero-knowledge-vault-trust-model' },
      { label: 'ADR-0011: TEE Provider SPI', href: '/adr/0011-tee-provider-spi-and-confidential-space' },
      { label: 'ADR-0012: Auto-Escrow & Session Lifetimes', href: '/adr/0012-auto-escrow-and-decoupled-session-lifetimes' },
      { label: 'ADR-0013: Local Policy-Enforced Shell', href: '/adr/0013-local-policy-enforced-shell' },
      { label: 'ADR-0014: aileron.yaml Policy Schema', href: '/adr/0014-aileron-yaml-policy-schema' },
      { label: 'ADR-0015: Built-in Policy Defaults', href: '/adr/0015-built-in-policy-defaults' },
      { label: 'ADR-0016: Layer Overrides & Specificity', href: '/adr/0016-layer-overrides-and-specificity' },
      { label: 'ADR-0017: Pluggable Agent SPI', href: '/adr/0017-pluggable-agent-spi' }
    ]
  }
];

/**
 * Standalone nav entries rendered below the main tree (e.g. API Reference).
 */
export const externalLinks: NavLink[] = [
  { label: 'API Reference', href: '/api' }
];
