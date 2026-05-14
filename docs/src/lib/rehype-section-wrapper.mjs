// Rehype plugin: wrap each <h2> and the siblings following it (up to but
// not including the next <h2>, <SectionBreak />, or <SectionResume />)
// in a <section class="prose-section">. Content above the first <h2>
// stays unwrapped at the root.
//
// Authors use:
//   <SectionBreak />   to close the current section (subsequent content
//                      renders unwrapped until the next h2).
//   <SectionResume />  to start a new section without an h2 (useful after
//                      a SectionBreak, or to open a section before any h2).
//
// Both marker components are no-ops at render time; this plugin detects
// them in the hast tree and rewrites the structure accordingly.

export default function rehypeSectionWrapper() {
	return (tree) => {
		if (tree.type !== 'root' || !Array.isArray(tree.children)) return;

		const isH2 = (node) =>
			node.type === 'element' && node.tagName === 'h2';

		// MDX JSX components survive to the hast tree as mdxJsxFlowElement
		// or mdxJsxTextElement with a `name` property. Markers are detected
		// by name and removed from the output.
		const isMarker = (node, name) =>
			(node.type === 'mdxJsxFlowElement' ||
				node.type === 'mdxJsxTextElement') &&
			node.name === name;

		const newSection = (children) => ({
			type: 'element',
			tagName: 'section',
			properties: { className: ['prose-section', 'ink-bleed'] },
			children,
		});

		const out = [];
		let currentSection = null;
		const flush = () => {
			if (currentSection) {
				out.push(currentSection);
				currentSection = null;
			}
		};

		for (const child of tree.children) {
			if (isH2(child)) {
				flush();
				currentSection = newSection([child]);
			} else if (isMarker(child, 'SectionResume')) {
				// Start a new empty section. Subsequent content fills it.
				// The marker itself is dropped.
				flush();
				currentSection = newSection([]);
			} else if (isMarker(child, 'SectionBreak')) {
				// Close the current section. The marker itself is dropped.
				// Subsequent content sits at the root until the next h2
				// or SectionResume.
				flush();
			} else if (currentSection) {
				currentSection.children.push(child);
			} else {
				out.push(child);
			}
		}

		flush();
		tree.children = out;
	};
}
