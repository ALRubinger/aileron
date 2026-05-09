// Rehype plugin: wrap each <pre> code block in a positioned container and
// inject a "Copy" button. The button itself does no work in the AST; a small
// global script (loaded by BaseLayout) wires the click to navigator.clipboard.
//
// Why server-rendered: shipping the button in the static HTML keeps it visible
// on first paint with no client-render flash, and it degrades to plain text if
// JS fails (the user can still select-and-copy).
//
// Scope: only top-level <pre> elements that contain <code> are wrapped. Inline
// <code> spans, prose <pre> without code, and any <pre> already inside a
// `.code-block-wrapper` are left alone (the last guard is for re-runs in MDX).

import { visit } from 'unist-util-visit';

// Lucide icon SVGs (16x16, currentColor stroke), inlined as hast nodes
// so they ship in the static HTML next to the button. Kept in sync
// with the lucide-svelte components used in code-block.svelte so both
// sides of the runtime/build-time fence look identical.

const SVG_BASE_PROPS = {
  xmlns: 'http://www.w3.org/2000/svg',
  width: 16,
  height: 16,
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  'stroke-width': 2,
  'stroke-linecap': 'round',
  'stroke-linejoin': 'round',
  'aria-hidden': 'true',
};

const clipboardIcon = {
  type: 'element',
  tagName: 'svg',
  properties: { ...SVG_BASE_PROPS, className: ['icon-clipboard'] },
  children: [
    {
      type: 'element',
      tagName: 'rect',
      properties: { width: 8, height: 4, x: 8, y: 2, rx: 1, ry: 1 },
      children: [],
    },
    {
      type: 'element',
      tagName: 'path',
      properties: {
        d: 'M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2',
      },
      children: [],
    },
  ],
};

const checkIcon = {
  type: 'element',
  tagName: 'svg',
  properties: { ...SVG_BASE_PROPS, className: ['icon-check'] },
  children: [
    {
      type: 'element',
      tagName: 'path',
      properties: { d: 'M20 6 9 17l-5-5' },
      children: [],
    },
  ],
};

/**
 * @returns {import('unified').Plugin}
 */
export default function rehypeCopyButton() {
  return (tree) => {
    visit(tree, 'element', (node, index, parent) => {
      if (!parent || index === undefined || index === null) return;
      if (node.tagName !== 'pre') return;

      // Skip <pre> that doesn't wrap a <code> child (rare in markdown, but
      // possible in raw HTML).
      const codeChild = (node.children || []).find(
        (c) => c.type === 'element' && c.tagName === 'code',
      );
      if (!codeChild) return;

      // Skip code blocks with no language. Convention: a fenced block
      // with a language (```sh, ```js, ...) is a command intended to
      // be run, so it gets a copy button. A bare ``` block is a sample
      // of console output / expected text, not something to copy and
      // execute, so it stays unadorned.
      //
      // Two pipelines reach this plugin:
      //   1. Plain remark-rehype puts the language as a class on
      //      <code>: `<code class="language-sh">`.
      //   2. Astro's Shiki highlighter (the project default) runs
      //      before user rehype plugins and rewrites the structure:
      //      it moves the language to a data attribute on <pre>
      //      (`<pre data-language="sh" class="astro-code">`) and
      //      strips `language-X` from <code>.
      // Check both forms so the plugin works in either pipeline and
      // remains correct if syntax highlighting is later disabled.
      const preProps = node.properties || {};
      const preDataLanguage =
        preProps['data-language'] || preProps.dataLanguage;
      const codeClass =
        (codeChild.properties && codeChild.properties.className) || [];
      const codeClasses = Array.isArray(codeClass)
        ? codeClass
        : String(codeClass).split(/\s+/);
      const hasLanguageClass = codeClasses.some((c) =>
        String(c).startsWith('language-'),
      );
      // Shiki labels unfenced ``` blocks `data-language="plaintext"`,
      // not absent — treat plaintext as "no language" so console
      // output samples stay unadorned.
      const isPlaintext =
        typeof preDataLanguage === 'string' &&
        preDataLanguage.toLowerCase() === 'plaintext';
      const hasLanguage = (!!preDataLanguage && !isPlaintext) || hasLanguageClass;
      if (!hasLanguage) return;

      // Skip if we've already wrapped this <pre> (idempotency).
      const parentClass =
        (parent.type === 'element' &&
          parent.properties &&
          parent.properties.className) ||
        [];
      const parentClasses = Array.isArray(parentClass)
        ? parentClass
        : String(parentClass).split(/\s+/);
      if (parentClasses.includes('code-block-wrapper')) return;

      const button = {
        type: 'element',
        tagName: 'button',
        properties: {
          type: 'button',
          className: ['code-copy-button'],
          'aria-label': 'Copy code to clipboard',
        },
        children: [
          // Deep-clone the icon nodes so the AST stays a tree, not a DAG.
          JSON.parse(JSON.stringify(clipboardIcon)),
          JSON.parse(JSON.stringify(checkIcon)),
        ],
      };

      const wrapper = {
        type: 'element',
        tagName: 'div',
        properties: { className: ['code-block-wrapper'] },
        children: [button, node],
      };

      parent.children[index] = wrapper;
      // Don't descend into the wrapper we just created.
      return [visit.SKIP, index + 1];
    });
  };
}
