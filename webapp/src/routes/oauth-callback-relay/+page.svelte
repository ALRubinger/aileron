<script lang="ts">
	import { onMount } from 'svelte';

	// OAuth callback relay (#743 PR 3). The daemon-hosted callback
	// (PR #745, internal/app/handlers_bindings_oauth2.go) writes the
	// binding then 303s the user's browser to a loopback-only
	// return_to URL with a fragment of the form
	//   #aileron_oauth=ok&binding=<urlencoded binding name>
	// or
	//   #aileron_oauth=error
	//
	// The webapp's stale-binding panel runs the reauthorize flow in a
	// popup (so the install modal's state survives the OAuth dance).
	// The popup ends up here on completion. This page parses the
	// fragment, posts the result to the opener via postMessage, and
	// closes itself. The opener listens for the message and updates
	// the corresponding stale-binding row.
	//
	// All work happens client-side — the daemon's callback already
	// committed the binding before the redirect landed.

	let parsed = $state<{ status: string; binding: string } | null>(null);
	let postedToOpener = $state(false);
	let closingSelf = $state(false);
	let manualClose = $state(false);

	function parseFragment(hash: string): { status: string; binding: string } {
		// Strip the leading "#"; the daemon-side appender builds the
		// fragment shape so this parser only needs to handle k=v&k=v.
		const trimmed = hash.replace(/^#/, '');
		const params = new URLSearchParams(trimmed);
		return {
			status: params.get('aileron_oauth') ?? '',
			binding: params.get('binding') ?? ''
		};
	}

	onMount(() => {
		parsed = parseFragment(window.location.hash);
		const opener = window.opener;
		if (!opener) {
			// User landed here without an opener — surface the result
			// in-page instead of leaving them on a blank screen. Same
			// content the popup path would have posted; the opener-
			// recovery story is "click the modal again", same as a
			// CLI session restart.
			manualClose = true;
			return;
		}
		try {
			// Targeting the same-origin opener; the daemon validated
			// return_to as loopback-only at init time, and the relay
			// only ever lands on the webapp's own origin.
			opener.postMessage(
				{
					type: 'aileron-oauth-callback',
					status: parsed.status,
					binding: parsed.binding
				},
				window.location.origin
			);
			postedToOpener = true;
		} catch {
			// postMessage failed (cross-origin, opener crashed, etc.)
			// — keep the page rendered so the user has something to
			// read rather than a blank flash before close.
			manualClose = true;
			return;
		}
		closingSelf = true;
		// Give the postMessage event loop a tick to flush before the
		// window closes. 50ms is invisible to the user and reliably
		// avoids dropping the message in slow tabs.
		setTimeout(() => window.close(), 50);
	});
</script>

<svelte:head>
	<title>Aileron OAuth callback</title>
</svelte:head>

<main class="mx-auto max-w-md p-6 text-sm font-sans">
	{#if !parsed}
		<p>Completing OAuth callback…</p>
	{:else if closingSelf}
		<p>OAuth callback received. Closing this window…</p>
	{:else if manualClose}
		{#if parsed.status === 'ok'}
			<h1 class="mb-2 text-base font-medium">Reauthorized {parsed.binding || 'binding'}.</h1>
			<p>You can close this tab and return to Aileron.</p>
		{:else}
			<h1 class="mb-2 text-base font-medium">Reauthorize was not completed.</h1>
			<p>Close this tab and try again from the Aileron install modal.</p>
		{/if}
	{:else}
		<p>OAuth callback received.</p>
	{/if}
</main>
