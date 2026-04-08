import { getAdrList } from '$lib/adr';

export function load() {
	return {
		adrs: getAdrList()
	};
}
