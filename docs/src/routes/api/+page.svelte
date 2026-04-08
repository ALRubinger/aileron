<script lang="ts">
	import { onMount } from 'svelte';

	onMount(() => {
		const container = document.getElementById('scalar-container');
		if (!container) return;

		const ref = document.createElement('script');
		ref.id = 'api-reference';
		ref.dataset.url = '/openapi.yaml';
		container.appendChild(ref);

		const cdn = document.createElement('script');
		cdn.src = 'https://cdn.jsdelivr.net/npm/@scalar/api-reference';
		document.head.appendChild(cdn);
	});
</script>

<svelte:head>
	<title>API Reference - Aileron Docs</title>
</svelte:head>

<div id="scalar-container" class="not-prose"></div>

<style>
	#scalar-container {
		margin: -2rem -1.5rem;
		min-height: 100vh;
	}
</style>
