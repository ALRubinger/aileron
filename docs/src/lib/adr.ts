const adrFiles = import.meta.glob('/adr/*.md', { eager: true, query: '?raw', import: 'default' });

export interface AdrEntry {
	slug: string;
	title: string;
}

export function getAdrList(): AdrEntry[] {
	return Object.entries(adrFiles)
		.map(([path, content]) => {
			const slug = path.split('/').pop()!.replace('.md', '');
			const firstLine = (content as string).split('\n')[0];
			const title = firstLine.replace(/^#\s*/, '');
			return { slug, title };
		})
		.sort((a, b) => a.slug.localeCompare(b.slug));
}

export function getAdr(slug: string): string | null {
	const content = adrFiles[`/adr/${slug}.md`] as string | undefined;
	return content ?? null;
}
