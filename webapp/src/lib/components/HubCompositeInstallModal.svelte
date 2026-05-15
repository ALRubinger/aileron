<script lang="ts">
	import * as Dialog from '$lib/components/ui/dialog';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import {
		getHubActionInstallDecision,
		getHubSuiteInstallDecision,
		installAction,
		type ConfirmedFingerprint,
		type HubActionInstallDecision,
		type HubSuiteInstallDecision,
		type HubInstallAuthority,
		type HubTrustState,
		type InstalledActionResult,
		type StaleBinding,
		type UnboundCapability
	} from '$lib/api';

	// Composite install modal (#709, action-first amendment). Opened
	// when the user picks a Hub-listed action or suite to install. The
	// modal fetches the composite install-decision payload from the
	// daemon, renders one panel per unique connector authority, and —
	// when the operator confirms — drives the install end-to-end from
	// the browser via /v1/actions/install + confirmed_fingerprints[]
	// (#743).
	//
	// Suite installs walk each member action client-side, accumulating
	// unbound_capabilities and newly_stale_bindings across the results
	// so the final success panel reflects the whole suite, not just
	// the last action.

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

	// Install state machine. `idle` → user can click Install when the
	// trust panel + version are in hand; `running` → install in flight,
	// button disabled; `done` → success panel with results; `failed`
	// → error message + retry. Separating these from decisionError
	// makes the install path testable independently and keeps the
	// trust-panel render unchanged when an install fails.
	type InstallPhase = 'idle' | 'running' | 'done' | 'failed';
	let installPhase = $state<InstallPhase>('idle');
	let installError = $state('');
	let installResults = $state<InstalledActionResult[]>([]);
	let installProgress = $state(''); // e.g. "Installing 2 of 5: <fqn>"

	// Reset install state when the operator opens the modal for a
	// different fqn — avoids surfacing the previous install's panel
	// against a freshly-fetched decision.
	$effect(() => {
		if (!fqn || !kind) {
			decision = null;
			decisionError = '';
			installPhase = 'idle';
			installError = '';
			installResults = [];
			installProgress = '';
			return;
		}
		loading = true;
		decision = null;
		decisionError = '';
		installPhase = 'idle';
		installError = '';
		installResults = [];
		installProgress = '';
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
		// Fallback affordance for operators who'd rather complete the
		// install in their terminal. Pins to `@latest` because the CLI
		// resolves it via the releases API the same way the daemon does.
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

	// fingerprintsFromDecision maps the composite payload's authorities
	// into the request shape /v1/actions/install accepts. Each entry
	// becomes a (authority_fqn, fingerprint) pair; the daemon iterates
	// and writes per-authority trust to the keyring before running the
	// install pipeline (#743).
	function fingerprintsFromDecision(d: Decision): ConfirmedFingerprint[] {
		return d.authorities.map((a) => ({
			fqn: a.fqn,
			fingerprint: a.fingerprint
		}));
	}

	// installableNow reports whether the modal has enough state to
	// dispatch the install: trust panel loaded, no decision error,
	// not already in flight, and the daemon resolved a concrete
	// version. The version requirement is the load-bearing piece —
	// /v1/actions/install rejects "latest" — so the button stays
	// disabled when the source repo has no published releases and
	// the operator falls back to the CLI affordance below.
	const installableNow = $derived(
		!!decision &&
			!decisionError &&
			installPhase !== 'running' &&
			!!decision.latest_version
	);

	// Aggregated stale + unbound from all installs in this modal
	// invocation. Dedupe by binding name (suite installs commonly fire
	// the same stale-binding transition across N actions on the same
	// connector — surface it once).
	const aggregatedStale = $derived.by<StaleBinding[]>(() => {
		const seen = new Set<string>();
		const out: StaleBinding[] = [];
		for (const r of installResults) {
			for (const s of r.newly_stale_bindings ?? []) {
				if (seen.has(s.name)) continue;
				seen.add(s.name);
				out.push(s);
			}
		}
		return out;
	});
	const aggregatedUnbound = $derived.by<UnboundCapability[]>(() => {
		const seen = new Set<string>();
		const out: UnboundCapability[] = [];
		for (const r of installResults) {
			for (const u of r.unbound_capabilities ?? []) {
				const k = u.connector_fqn + '|' + u.kind;
				if (seen.has(k)) continue;
				seen.add(k);
				out.push(u);
			}
		}
		return out;
	});

	// runInstall dispatches the install based on `kind`. Action: one
	// /v1/actions/install call. Suite: loop through member_actions in
	// declaration order, sharing the same confirmed_fingerprints[] on
	// each call. The daemon's confirmHubTrust short-circuits on the
	// second-and-later calls once the keyring has the publisher key,
	// so the per-call overhead is one keyring lookup, not a refetch.
	async function runInstall() {
		if (!decision || !decision.latest_version) return;
		const version = decision.latest_version;
		const fingerprints = fingerprintsFromDecision(decision);
		installPhase = 'running';
		installError = '';
		installResults = [];
		try {
			if (decision.kind === 'action') {
				installProgress = `Installing ${decision.fqn}`;
				const r = await installAction({
					fqn: decision.fqn,
					version,
					auto_install_connectors: true,
					confirmed_fingerprints: fingerprints
				});
				installResults = [r];
			} else {
				const total = decision.member_actions.length;
				const acc: InstalledActionResult[] = [];
				for (let i = 0; i < total; i++) {
					const memberFQN = decision.member_actions[i];
					installProgress = `Installing ${i + 1} of ${total}: ${memberFQN}`;
					const r = await installAction({
						fqn: memberFQN,
						version,
						auto_install_connectors: true,
						confirmed_fingerprints: fingerprints
					});
					acc.push(r);
					// Push partial results so a mid-loop failure still
					// shows what landed before the abort.
					installResults = [...acc];
				}
			}
			installPhase = 'done';
			installProgress = '';
		} catch (e: unknown) {
			installError = e instanceof Error ? e.message : String(e);
			installPhase = 'failed';
			installProgress = '';
		}
	}
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

				{#if installPhase !== 'done'}
					<div class="space-y-2 border-t border-border pt-3" data-testid="hub-composite-install-panel">
						<p class="text-xs font-medium uppercase tracking-wide text-muted-foreground">
							Install
						</p>
						{#if !decision.latest_version}
							<p class="text-xs text-destructive" data-testid="hub-composite-no-version">
								The source repo has no published releases. Install from the
								CLI by pinning a specific tag or commit:
							</p>
						{:else}
							<p class="text-xs text-muted-foreground">
								The daemon writes per-repo trust to your keyring for every
								authority listed above, then runs the install pipeline. You
								can review what landed in the success panel.
							</p>
						{/if}
						{#if installPhase === 'failed'}
							<p class="text-xs text-destructive" data-testid="hub-composite-install-error">
								Install failed: {installError}
							</p>
						{/if}
						{#if installPhase === 'running'}
							<p class="text-xs text-muted-foreground" data-testid="hub-composite-install-progress">
								{installProgress || 'Working…'}
							</p>
						{/if}
						<div class="flex flex-wrap items-center gap-2">
							<Button
								variant="default"
								disabled={!installableNow}
								onclick={runInstall}
								data-testid="hub-composite-install-button"
							>
								{#if installPhase === 'running'}
									Installing…
								{:else if installPhase === 'failed'}
									Retry install
								{:else if decision.kind === 'suite'}
									Install suite ({decision.member_actions.length})
								{:else}
									Install action
								{/if}
							</Button>
							{#if decision.latest_version}
								<span class="text-xs text-muted-foreground" data-testid="hub-composite-resolved-version">
									v{decision.latest_version}
								</span>
							{/if}
						</div>
						<details class="text-xs">
							<summary class="cursor-pointer text-muted-foreground">
								Or copy the CLI command
							</summary>
							<pre
								class="mt-2 rounded border border-border bg-muted/50 p-2 font-mono whitespace-pre-wrap break-all"
								data-testid="hub-composite-cli-command"><code>{cliCommand(decision)}</code></pre>
						</details>
					</div>
				{:else}
					<div class="space-y-3 border-t border-border pt-3" data-testid="hub-composite-success">
						<p class="text-xs font-medium uppercase tracking-wide text-muted-foreground">
							Install complete
						</p>
						<ul class="space-y-1 text-xs" data-testid="hub-composite-installed-list">
							{#each installResults as r (r.name)}
								<li>
									<span class="font-mono">{r.name}</span>
									<span class="text-muted-foreground"> v{r.version}</span>
									{#if r.already_installed}
										<span class="text-muted-foreground"> · already installed</span>
									{/if}
								</li>
							{/each}
						</ul>
						{#if aggregatedUnbound.length > 0}
							<div class="rounded border border-border bg-muted/30 p-2 text-xs" data-testid="hub-composite-unbound">
								<p class="font-medium">Unbound credentials</p>
								<p class="text-muted-foreground">
									These connectors need a credential before the action can run.
									Bind them at the Connected Accounts page or via
									<code class="font-mono">aileron binding setup</code>.
								</p>
								<ul class="mt-1 ml-4 list-disc">
									{#each aggregatedUnbound as u (u.connector_fqn + '|' + u.kind)}
										<li class="font-mono break-all">
											{u.connector_fqn} ({u.kind}{#if u.scope}, {u.scope}{/if})
										</li>
									{/each}
								</ul>
							</div>
						{/if}
						{#if aggregatedStale.length > 0}
							<div class="rounded border border-yellow-500/50 bg-yellow-500/5 p-2 text-xs" data-testid="hub-composite-stale">
								<p class="font-medium">Reauthorization required</p>
								<p class="text-muted-foreground">
									This install upgraded a connector whose new manifest needs
									additional OAuth scopes (or an existing binding predates
									scope tracking). Reauthorize each binding before invoking
									the action.
								</p>
								<ul class="mt-1 space-y-1">
									{#each aggregatedStale as s (s.name)}
										<li>
											<span class="font-mono break-all">{s.name}</span>
											{#if s.service}<span class="text-muted-foreground"> ({s.service})</span>{/if}
											{#if s.stale_reason === 'scope_drift' && s.missing_scopes && s.missing_scopes.length > 0}
												<div class="ml-4 text-muted-foreground">
													Missing: {s.missing_scopes.join(', ')}
												</div>
											{:else if s.stale_reason === 'no_grant_record'}
												<div class="ml-4 text-muted-foreground">
													Predates scope tracking; one-time reauthorize needed.
												</div>
											{/if}
											<code class="ml-4 font-mono">aileron binding reauthorize {s.name}</code>
										</li>
									{/each}
								</ul>
							</div>
						{/if}
					</div>
				{/if}
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
