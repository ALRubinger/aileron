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
      const hasCodeChild = (node.children || []).some(
        (c) => c.type === 'element' && c.tagName === 'code',
      );
      if (!hasCodeChild) return;

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
          'data-copy-label': 'Copy',
          'data-copy-label-copied': 'Copied',
        },
        children: [{ type: 'text', value: 'Copy' }],
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
