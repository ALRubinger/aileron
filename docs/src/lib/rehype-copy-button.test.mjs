// Smoke test for rehype-copy-button. Builds a minimal HAST that mirrors what
// remark-rehype produces for a fenced code block, runs the plugin against it,
// and asserts the contract:
//   - <pre> with a <code> child is wrapped in <div class="code-block-wrapper">
//   - the wrapper contains a <button class="code-copy-button"> sibling of <pre>
//   - <pre> without <code> is left alone
//   - re-running the plugin doesn't double-wrap (idempotency)
//
// Run with: node --test docs/src/lib/rehype-copy-button.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import rehypeCopyButton from './rehype-copy-button.mjs';

function makeTree(children) {
  return { type: 'root', children };
}

function preWithCode(text, lang = 'sh') {
  return {
    type: 'element',
    tagName: 'pre',
    properties: {},
    children: [
      {
        type: 'element',
        tagName: 'code',
        properties: lang ? { className: [`language-${lang}`] } : {},
        children: [{ type: 'text', value: text }],
      },
    ],
  };
}

function run(tree) {
  const transformer = rehypeCopyButton();
  transformer(tree);
  return tree;
}

test('wraps a <pre><code> in .code-block-wrapper with a copy button', () => {
  const tree = makeTree([preWithCode('git status')]);
  run(tree);
  assert.equal(tree.children.length, 1);
  const wrapper = tree.children[0];
  assert.equal(wrapper.tagName, 'div');
  assert.deepEqual(wrapper.properties.className, ['code-block-wrapper']);
  assert.equal(wrapper.children.length, 2);
  const [button, pre] = wrapper.children;
  assert.equal(button.tagName, 'button');
  assert.equal(button.properties.type, 'button');
  assert.deepEqual(button.properties.className, ['code-copy-button']);
  assert.equal(button.properties['aria-label'], 'Copy code to clipboard');
  assert.equal(pre.tagName, 'pre');
  assert.equal(pre.children[0].tagName, 'code');
});

test('wraps Shiki-shaped <pre data-language="sh"><code> (post-highlight)', () => {
  // After Astro's Shiki transformer, the language has moved from
  // <code class="language-sh"> to <pre data-language="sh" class="astro-code">.
  const shikiPre = {
    type: 'element',
    tagName: 'pre',
    properties: {
      'data-language': 'sh',
      className: ['astro-code'],
    },
    children: [
      {
        type: 'element',
        tagName: 'code',
        properties: {},
        children: [{ type: 'text', value: 'aileron version' }],
      },
    ],
  };
  const tree = makeTree([shikiPre]);
  run(tree);
  assert.equal(tree.children.length, 1);
  assert.equal(tree.children[0].tagName, 'div');
  assert.deepEqual(tree.children[0].properties.className, ['code-block-wrapper']);
});

test('leaves a <pre><code> without language class alone (output sample, not a command)', () => {
  const tree = makeTree([preWithCode('Output line 1\nOutput line 2', null)]);
  run(tree);
  assert.equal(tree.children.length, 1);
  assert.equal(tree.children[0].tagName, 'pre');
});

test('leaves a Shiki <pre data-language="plaintext"> alone (unfenced ``` block)', () => {
  // Shiki labels every unfenced ``` block `data-language="plaintext"`.
  // We treat that as "no language" so console output stays unadorned.
  const plaintextPre = {
    type: 'element',
    tagName: 'pre',
    properties: {
      'data-language': 'plaintext',
      className: ['astro-code'],
    },
    children: [
      {
        type: 'element',
        tagName: 'code',
        properties: {},
        children: [{ type: 'text', value: 'aileron dev (abc1234)' }],
      },
    ],
  };
  const tree = makeTree([plaintextPre]);
  run(tree);
  assert.equal(tree.children.length, 1);
  assert.equal(tree.children[0].tagName, 'pre');
});

test('leaves a <pre> without <code> child alone', () => {
  const bare = {
    type: 'element',
    tagName: 'pre',
    properties: {},
    children: [{ type: 'text', value: 'plain' }],
  };
  const tree = makeTree([bare]);
  run(tree);
  assert.equal(tree.children.length, 1);
  assert.equal(tree.children[0].tagName, 'pre');
});

test('does not wrap inline <code>', () => {
  const inline = {
    type: 'element',
    tagName: 'p',
    properties: {},
    children: [
      { type: 'text', value: 'Use ' },
      {
        type: 'element',
        tagName: 'code',
        properties: {},
        children: [{ type: 'text', value: 'jq' }],
      },
      { type: 'text', value: '.' },
    ],
  };
  const tree = makeTree([inline]);
  run(tree);
  assert.equal(tree.children.length, 1);
  assert.equal(tree.children[0].tagName, 'p');
});

test('is idempotent across reruns', () => {
  const tree = makeTree([preWithCode('echo hi')]);
  run(tree);
  run(tree);
  assert.equal(tree.children.length, 1);
  const wrapper = tree.children[0];
  assert.deepEqual(wrapper.properties.className, ['code-block-wrapper']);
  assert.equal(wrapper.children.length, 2);
  assert.equal(wrapper.children[0].tagName, 'button');
  assert.equal(wrapper.children[1].tagName, 'pre');
});

test('handles multiple <pre> blocks in the same tree', () => {
  const tree = makeTree([
    preWithCode('one'),
    {
      type: 'element',
      tagName: 'p',
      properties: {},
      children: [{ type: 'text', value: 'between' }],
    },
    preWithCode('two'),
  ]);
  run(tree);
  assert.equal(tree.children.length, 3);
  assert.deepEqual(tree.children[0].properties.className, [
    'code-block-wrapper',
  ]);
  assert.equal(tree.children[1].tagName, 'p');
  assert.deepEqual(tree.children[2].properties.className, [
    'code-block-wrapper',
  ]);
});
