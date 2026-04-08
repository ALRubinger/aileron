<script lang="ts">
	import { marked } from 'marked';

	let { data } = $props();

	// Rewrite relative .md links to site paths
	const renderer = new marked.Renderer();
	const originalLink = renderer.link.bind(renderer);
	renderer.link = function ({ href, title, tokens }) {
		if (href && href.endsWith('.md') && !href.startsWith('http')) {
			href = '/adr/' + href.replace('.md', '');
		}
		return originalLink({ href, title, tokens });
	};

	const html = $derived(marked(data.content, { renderer }) as string);
</script>

<svelte:head>
	<title>{data.slug} - Aileron Docs</title>
</svelte:head>

<p><a href="/adr">&larr; All ADRs</a></p>

{@html html}
