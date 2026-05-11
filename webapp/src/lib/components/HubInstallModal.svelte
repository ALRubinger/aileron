<script lang="ts">
	import * as Dialog from '$lib/components/ui/dialog';
	import { Badge } from '$lib/components/ui/badge';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import {
		getHubInstallDecision,
		installConnector,
		type HubInstallDecision,
		type InstalledConnector
	} from '$lib/api';

	// Hub install modal (ADR-0013, #488). Opened when the user picks a
	// Hub-listed connector to install from the browse page. The modal
	// fetches the install-decision payload — same one the CLI's
	// `aileron connector install` renders — and lets the user confirm
	// the publisher's key fingerprint before running the install
	// pipeline. On confirm, the install POST carries
	// `confirmed_fingerprint`; the daemon writes per-FQN trust to the
	// keyring before installing (#487 Q4 resolution).
	//
	// Per ADR-0013, the modal renders publisher info as informational
	// context only — there is no "Trust this publisher" button in v0.x.
	// Trust granularity stays strictly per-repo.

	type Props = {
		fqn: string | null;
		onClose: () => void;
	};

	let { fqn, onClose }: Props = $props();

	const open = $derived(fqn !== null);

	let decision = $state<HubInstallDecision | null>(null);
	let decisionError = $state('');
	let loading = $state(false);

	let version = $state('');
	let expectedHash = $state('');
	let installing = $state(false);
	let installError = $state('');
	let installed = $state<InstalledConnector | null>(null);

	// Re-fetch the install-decision every time the modal opens for a
	// new FQN. Resets all install-side state too so the previous
	// install's success message doesn't bleed into the next session.
	$effect(() => {
		if (!fqn) {
			decision = null;
			decisionError = '';
			version = '';
			expectedHash = '';
			installError = '';
			installed = null;
			return;
		}
		loading = true;
		decision = null;
		decisionError = '';
		installed = null;
		installError = '';
		const target = fqn;
		void getHubInstallDecision(target)
			.then((d) => {
				if (fqn === target) decision = d;
			})
			.catch((e: unknown) => {
				if (fqn === target) {
					decisionError = e instanceof Error ? e.message : String(e);
				}
			})
			.finally(() => {
				if (fqn === target) loading = false;
			});
	});

	function handleOpenChange(next: boolean) {
		if (!next) {
			onClose();
		}
	}

	async function handleInstall() {
		if (!decision || !version.trim() || installing) return;
		installing = true;
		installError = '';
		try {
			installed = await installConnector({
				fqn: decision.fqn,
				version: version.trim(),
				confirmed_fingerprint: decision.fingerprint,
				expected_hash: expectedHash.trim() || undefined
			});
		} catch (e) {
			installError = e instanceof Error ? e.message : String(e);
		} finally {
			installing = false;
		}
	}

	function trustBadgeVariant(
		state: HubInstallDecision['trust_state']
	): 'default' | 'secondary' | 'destructive' | 'outline' {
		// Match the CLI's color semantics from main.go:colorizeTrustState:
		// already_trusted = green-equivalent, unknown = informational,
		// conflict = destructive. The Badge primitive doesn't ship a
		// "green" variant, so the green-equivalent rides on `default`
		// (primary), which is the strong-affirmative color in the
		// active theme.
		switch (state) {
			case 'already_trusted':
				return 'default';
			case 'conflict':
				return 'destructive';
			default:
				return 'secondary';
		}
	}

	function trustLabel(state: HubInstallDecision['trust_state']): string {
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
</script>

<Dialog.Root {open} onOpenChange={handleOpenChange}>
	<Dialog.Content data-testid="hub-install-modal">
		<Dialog.Header>
			<Dialog.Title>Install from the Hub</Dialog.Title>
			<Dialog.Description>
				Review the publisher's key fingerprint before installing. The daemon
				will write per-repo trust to your keyring on confirm.
			</Dialog.Description>
		</Dialog.Header>

		{#if loading}
			<p class="text-sm text-muted-foreground">Loading install decision…</p>
		{:else if decisionError}
			<p class="text-sm text-destructive" data-testid="hub-install-decision-error">
				{decisionError}
			</p>
		{:else if decision && installed}
			<div class="space-y-2 text-sm" data-testid="hub-install-installed">
				<p class="text-foreground">
					<span class="font-semibold">
						{installed.already_installed ? 'Already installed' : 'Installed'}:
					</span>
					{installed.fqn}@{installed.version}
				</p>
				<dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-xs">
					<dt class="text-muted-foreground">Hash</dt>
					<dd class="font-mono break-all">{installed.hash}</dd>
					<dt class="text-muted-foreground">Path</dt>
					<dd class="font-mono break-all">{installed.entry_dir}</dd>
				</dl>
			</div>
			<Dialog.Footer>
				<Button variant="default" onclick={onClose}>Close</Button>
			</Dialog.Footer>
		{:else if decision}
			<div class="space-y-3 text-sm" data-testid="hub-install-decision">
				<dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
					<dt class="text-muted-foreground">FQN</dt>
					<dd class="font-mono break-all">{decision.fqn}</dd>
					{#if decision.description}
						<dt class="text-muted-foreground">Description</dt>
						<dd>{decision.description}</dd>
					{/if}
					<dt class="text-muted-foreground">Publisher</dt>
					<dd>{decision.publisher_github}</dd>
					<dt class="text-muted-foreground">Fingerprint</dt>
					<dd class="font-mono text-xs break-all">{decision.fingerprint}</dd>
					<dt class="text-muted-foreground">Trust</dt>
					<dd>
						<Badge
							variant={trustBadgeVariant(decision.trust_state)}
							data-testid="hub-trust-state"
							data-trust-state={decision.trust_state}
						>
							{trustLabel(decision.trust_state)}
						</Badge>
					</dd>
				</dl>

				{#if decision.risk_indicators.length > 0}
					<div data-testid="hub-risk-indicators">
						<p class="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">
							Risk indicators
						</p>
						<ul class="space-y-1 text-xs">
							{#each decision.risk_indicators as risk (risk)}
								<li
									class={decision.trust_state === 'conflict'
										? 'text-destructive'
										: 'text-foreground'}
								>
									{risk}
								</li>
							{/each}
						</ul>
					</div>
				{/if}

				{#if decision.publisher_footprint.length > 0}
					<div data-testid="hub-publisher-footprint">
						<p class="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">
							Other connectors by this publisher
						</p>
						<ul class="space-y-0.5 text-xs font-mono">
							{#each decision.publisher_footprint as fp (fp)}
								<li>{fp}</li>
							{/each}
						</ul>
					</div>
				{/if}

				<div class="space-y-2 border-t border-border pt-3">
					<label class="flex flex-col gap-1 text-xs">
						<span class="font-medium">Version</span>
						<Input
							type="text"
							placeholder="e.g. 1.2.0"
							bind:value={version}
							disabled={installing}
							data-testid="hub-install-version-input"
						/>
					</label>
					<label class="flex flex-col gap-1 text-xs">
						<span class="font-medium">Expected hash (optional)</span>
						<Input
							type="text"
							placeholder="sha256:…"
							bind:value={expectedHash}
							disabled={installing}
							data-testid="hub-install-hash-input"
						/>
					</label>
					{#if installError}
						<p class="text-xs text-destructive" data-testid="hub-install-error">
							{installError}
						</p>
					{/if}
				</div>
			</div>
			<Dialog.Footer>
				<Button
					variant="default"
					disabled={!version.trim() || installing}
					onclick={handleInstall}
					data-testid="hub-install-button"
				>
					{installing ? 'Installing…' : 'Install'}
				</Button>
				<Button variant="ghost" disabled={installing} onclick={onClose}>
					Cancel
				</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>
