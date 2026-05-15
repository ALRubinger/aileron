<script lang="ts">
	import * as Dialog from '$lib/components/ui/dialog';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import {
		getHubActionInstallDecision,
		getHubSuiteInstallDecision,
		type HubActionInstallDecision,
		type HubSuiteInstallDecision,
		type HubInstallAuthority,
		type HubTrustState
	} from '$lib/api';

	// Composite install modal (#709, action-first amendment). Opened
	// when the user picks a Hub-listed action or suite to install. The
	// modal fetches the composite install-decision payload from the
	// daemon and renders one panel per unique connector authority in
	// the dependency closure. All authorities share a single confirm
	// action — the umbrella's "one trust gate, not N" promise.
	//
	// End-to-end install execution from the webapp requires a daemon-
	// side trust endpoint that v0.x doesn't expose; the modal surfaces
	// the operator's CLI command so they complete the install in their
	// terminal once they've reviewed and accepted the trust panel.
	// Tracked as a follow-up; the trust panel render is the load-
	// bearing UX delivery for this iteration.

	type Kind = 'action' | 'suite';

	type Props = {
		fqn: string | null;
		kind: Kind | null;
		onClose: () => void;
	};

	let { fqn, kind, onClose }: Props = $props();

	const open = $derived(fqn !== null && kind !== null);

	type Decision = HubActionInstallDecision | HubSuiteInstallDecision;
	let decision = $state<Decision | null>(null);
	let decisionError = $state('');
	let loading = $state(false);

	$effect(() => {
		if (!fqn || !kind) {
			decision = null;
			decisionError = '';
			return;
		}
		loading = true;
		decision = null;
		decisionError = '';
		const target = fqn;
		const targetKind = kind;
		const fetcher =
			targetKind === 'action'
				? getHubActionInstallDecision
				: getHubSuiteInstallDecision;
		void fetcher(target)
			.then((d) => {
				if (fqn === target && kind === targetKind) decision = d;
			})
			.catch((e: unknown) => {
				if (fqn === target && kind === targetKind) {
					decisionError = e instanceof Error ? e.message : String(e);
				}
			})
			.finally(() => {
				if (fqn === target && kind === targetKind) loading = false;
			});
	});

	function handleOpenChange(next: boolean) {
		if (!next) onClose();
	}

	function trustBadgeVariant(
		state: HubTrustState
	): 'default' | 'secondary' | 'destructive' | 'outline' {
		switch (state) {
			case 'already_trusted':
				return 'default';
			case 'conflict':
				return 'destructive';
			default:
				return 'secondary';
		}
	}

	function trustLabel(state: HubTrustState): string {
		switch (state) {
			case 'already_trusted':
				return 'Already trusted';
			case 'unknown':
				return 'Unknown publisher';
			case 'conflict':
				return 'Conflict — key differs from a sibling repo';
			default:
				return state;
		}
	}

	function cliCommand(d: Decision): string {
		// Pin to "@latest" since the composite payload doesn't carry a
		// version. The CLI resolves @latest via the releases API for
		// actions and via the suite source repo for suites; same shape
		// the docs already use.
		if (d.kind === 'action') {
			return `aileron action add ${d.fqn}@latest`;
		}
		return `aileron action add-suite ${d.fqn}.toml@latest`;
	}

	// Suite FQNs in the Hub catalog are `<owner>/<repo>/suite` (or
	// `<owner>/<repo>/suites/<x>`); the CLI's `add-suite` takes the
	// path to the actual `suite.toml` file at a ref. The `cliCommand`
	// helper appends `.toml@latest` to bridge that gap. Local
	// (non-remote) suite installs aren't reachable from the Hub, so
	// the heuristic is safe for every code path the modal serves.
</script>

<Dialog.Root {open} onOpenChange={handleOpenChange}>
	<Dialog.Content data-testid="hub-composite-install-modal">
		<Dialog.Header>
			<Dialog.Title>
				{#if kind === 'suite'}
					Install suite from the Hub
				{:else}
					Install action from the Hub
				{/if}
			</Dialog.Title>
			<Dialog.Description>
				Review every connector authority this install depends on. One
				accept covers them all — the daemon writes per-repo trust to your
				keyring before the install pipeline runs.
			</Dialog.Description>
		</Dialog.Header>

		{#if loading}
			<p class="text-sm text-muted-foreground">Loading install decision…</p>
		{:else if decisionError}
			<p
				class="text-sm text-destructive"
				data-testid="hub-composite-decision-error"
			>
				{decisionError}
			</p>
		{:else if decision}
			<div class="space-y-4 text-sm" data-testid="hub-composite-decision">
				<dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
					<dt class="text-muted-foreground">
						{decision.kind === 'suite' ? 'Suite' : 'Action'}
					</dt>
					<dd class="font-mono break-all">{decision.fqn}</dd>
					{#if decision.description}
						<dt class="text-muted-foreground">Description</dt>
						<dd>{decision.description}</dd>
					{/if}
					<dt class="text-muted-foreground">Publisher</dt>
					<dd>{decision.publisher_github}</dd>
					{#if decision.kind === 'action'}
						<dt class="text-muted-foreground">Connector</dt>
						<dd class="font-mono break-all">{decision.connector_fqn}</dd>
					{/if}
				</dl>

				{#if decision.kind === 'suite' && decision.member_actions.length > 0}
					<div data-testid="hub-composite-members">
						<p
							class="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground"
						>
							Member actions ({decision.member_actions.length})
						</p>
						<ul class="space-y-0.5 text-xs font-mono">
							{#each decision.member_actions as a (a)}
								<li class="break-all">{a}</li>
							{/each}
						</ul>
					</div>
				{/if}

				<div data-testid="hub-composite-authorities">
					<p
						class="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground"
					>
						Trust gate ({decision.authorities.length})
					</p>
					<ul class="space-y-3">
						{#each decision.authorities as auth (auth.fqn)}
							{@render authorityPanel(auth)}
						{/each}
					</ul>
				</div>

				<div class="space-y-2 border-t border-border pt-3">
					<p
						class="text-xs font-medium uppercase tracking-wide text-muted-foreground"
					>
						Complete the install
					</p>
					<p class="text-xs text-muted-foreground">
						Run this in your terminal. The CLI walks the same trust panel
						you reviewed here and writes per-repo trust to your keyring on
						confirm.
					</p>
					<pre
						class="rounded border border-border bg-muted/50 p-2 text-xs font-mono whitespace-pre-wrap break-all"
						data-testid="hub-composite-cli-command"><code>{cliCommand(decision)}</code></pre>
				</div>
			</div>
			<Dialog.Footer>
				<Button variant="default" onclick={onClose}>Close</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>

{#snippet authorityPanel(auth: HubInstallAuthority)}
	<li
		class="rounded border border-border p-3 text-xs"
		data-testid="hub-composite-authority"
		data-fqn={auth.fqn}
	>
		<div class="font-mono break-all font-medium">{auth.fqn}</div>
		<div class="mt-1 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
			<span class="text-muted-foreground">Publisher</span>
			<span>{auth.publisher_github}</span>
			<span class="text-muted-foreground">Fingerprint</span>
			<span class="font-mono break-all">{auth.fingerprint}</span>
			<span class="text-muted-foreground">Trust</span>
			<span>
				<Badge
					variant={trustBadgeVariant(auth.trust_state)}
					data-testid="hub-composite-trust-state"
					data-trust-state={auth.trust_state}
				>
					{trustLabel(auth.trust_state)}
				</Badge>
			</span>
		</div>
		{#if auth.risk_indicators.length > 0}
			<ul
				class="mt-2 space-y-0.5"
				data-testid="hub-composite-risk-indicators"
			>
				{#each auth.risk_indicators as risk (risk)}
					<li
						class={auth.trust_state === 'conflict'
							? 'text-destructive'
							: 'text-foreground'}
					>
						{risk}
					</li>
				{/each}
			</ul>
		{/if}
		{#if auth.publisher_footprint.length > 0}
			<div class="mt-2">
				<p class="mb-1 text-muted-foreground">
					Other connectors by this publisher
				</p>
				<ul class="space-y-0.5 font-mono">
					{#each auth.publisher_footprint as fp (fp)}
						<li>{fp}</li>
					{/each}
				</ul>
			</div>
		{/if}
	</li>
{/snippet}
