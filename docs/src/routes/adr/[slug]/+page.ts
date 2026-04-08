import { error } from '@sveltejs/kit';
import { getAdr } from '$lib/adr';

export function load({ params }: { params: { slug: string } }) {
	const content = getAdr(params.slug);
	if (!content) {
		error(404, 'ADR not found');
	}
	return { content, slug: params.slug };
}
