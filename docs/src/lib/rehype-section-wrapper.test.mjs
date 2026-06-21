// Contract test for rehype-section-wrapper. Builds a minimal HAST that mirrors
// what remark-rehype produces, runs the plugin, and asserts the contract:
//   - each <h2> plus its following siblings (until the next <h2>) is wrapped in
//     a <section class="prose-section">
//   - the emitted section carries ONLY the prose-section class — the ink-bleed
//     effect has been removed from the design system, so no ink-bleed class is
//     ever emitted
//   - content before the first <h2> stays unwrapped at the root
//   - <SectionBreak /> closes the current section; <SectionResume /> opens one
//
// Run with: node --test docs/src/lib/rehype-section-wrapper.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import rehypeSectionWrapper from './rehype-section-wrapper.mjs';

function makeTree(children) {
	return { type: 'root', children };
}

function h2(text) {
	return {
		type: 'element',
		tagName: 'h2',
		properties: {},
		children: [{ type: 'text', value: text }],
	};
}

function p(text) {
	return {
		type: 'element',
		tagName: 'p',
		properties: {},
		children: [{ type: 'text', value: text }],
	};
}

function marker(name) {
	return { type: 'mdxJsxFlowElement', name, attributes: [], children: [] };
}

function run(tree) {
	const transformer = rehypeSectionWrapper();
	transformer(tree);
	return tree;
}

test('wraps an h2 and its following siblings in a prose-section', () => {
	const tree = makeTree([h2('Heading'), p('body one'), p('body two')]);
	run(tree);

	assert.equal(tree.children.length, 1);
	const section = tree.children[0];
	assert.equal(section.tagName, 'section');
	assert.equal(section.children.length, 3);
});

test('emits only the prose-section class — never ink-bleed', () => {
	const tree = makeTree([h2('Heading'), p('body')]);
	run(tree);

	const section = tree.children[0];
	assert.deepEqual(section.properties.className, ['prose-section']);
	assert.ok(
		!section.properties.className.includes('ink-bleed'),
		'ink-bleed must not appear on wrapped sections',
	);
});

test('starts a new section at each h2', () => {
	const tree = makeTree([h2('One'), p('a'), h2('Two'), p('b')]);
	run(tree);

	assert.equal(tree.children.length, 2);
	for (const section of tree.children) {
		assert.deepEqual(section.properties.className, ['prose-section']);
	}
});

test('leaves content before the first h2 unwrapped at the root', () => {
	const intro = p('intro paragraph');
	const tree = makeTree([intro, h2('Heading'), p('body')]);
	run(tree);

	assert.equal(tree.children.length, 2);
	assert.equal(tree.children[0], intro);
	assert.equal(tree.children[1].tagName, 'section');
});

test('SectionBreak closes the current section; SectionResume opens one', () => {
	const tree = makeTree([
		h2('Heading'),
		p('inside'),
		marker('SectionBreak'),
		p('outside'),
		marker('SectionResume'),
		p('inside again'),
	]);
	run(tree);

	// section(h2,inside) | p(outside) at root | section(inside again)
	assert.equal(tree.children.length, 3);
	assert.deepEqual(tree.children[0].properties.className, ['prose-section']);
	assert.equal(tree.children[1].tagName, 'p');
	assert.deepEqual(tree.children[2].properties.className, ['prose-section']);
});
