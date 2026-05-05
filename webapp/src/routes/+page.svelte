<script lang="ts">
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';

	// Landing card. Most visits to this webapp arrive via a notification
	// click that points at /approvals; this page is the "I clicked the
	// banner instead" fallback. Keeping it deliberately minimal — every
	// added link is one more thing to maintain, and the v0.x scope is
	// just the approvals queue.
</script>

<svelte:head>
	<title>Aileron — Local Webapp</title>
</svelte:head>

<div class="space-y-4">
	<h1 class="text-2xl font-semibold tracking-tight">Aileron</h1>
	<p class="text-muted-foreground">
		The local webapp surfaced under <code>aileron launch</code>. Use it to review and decide on
		action approvals the agent has paused on.
	</p>

	<Card.Root>
		<Card.Header>
			<Card.Title>Pending approvals</Card.Title>
		</Card.Header>
		<Card.Content class="flex items-center justify-between">
			<p class="text-sm text-muted-foreground">
				When the agent invokes an action gated by <code>[approval] required = true</code>, the
				request lands here for your decision. The page streams updates live — no refresh needed.
			</p>
			<Button>
				<a href="/approvals" class="text-current no-underline">Open</a>
			</Button>
		</Card.Content>
	</Card.Root>
</div>
