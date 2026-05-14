<script lang="ts">
	import {
		listInstalledActions,
		setActionEnabled,
		type InstalledAction
	} from '$lib/api';
	import { onMount } from 'svelte';
	import * as Card from '$lib/components/ui/card';

	// Installed-actions inventory. The daemon's GET /v1/actions already
	// merges the per-user overlay at `~/.aileron/action-state.json` into
	// each item's `enabled` field, so the page just renders what the
	// daemon returns and PATCHes back when the user flips a toggle.
	//
	// Hot reload is not in scope: the MCP server caches the tool list
	// at boot, so a toggle here only changes which tools the LLM sees
	// after the next MCP restart. The banner makes that explicit.
	let actions = $state<InstalledAction[]>([]);
	let loadErrors = $state<Array<{ file: string; message: string }>>([]);
	let loading = $state(true);
	let error = $state('');
	let toggling = $state<Record<string, boolean>>({});

	function isEnabled(a: InstalledAction): boolean {
		return a.enabled === undefined || a.enabled;
	}

	async function toggle(a: InstalledAction) {
		const next = !isEnabled(a);
		toggling[a.name] = true;
		try {
			const updated = await setActionEnabled(a.name, next);
			actions = actions.map((x) => (x.name === a.name ? updated : x));
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			toggling[a.name] = false;
		}
	}

	async function refresh() {
		loading = true;
		error = '';
		try {
			const resp = await listInstalledActions();
			actions = resp.items ?? [];
			loadErrors = (resp.load_errors ?? []).map((e) => ({
				file: e.file,
				message: e.message
			}));
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		void refresh();
	});
</script>

<svelte:head>
	<title>Actions — Aileron</title>
</svelte:head>

<h1 class="mb-4 text-3xl font-extrabold tracking-tight">Installed Actions</h1>

<p class="ink-bleed mb-3 text-sm text-muted-foreground">
	Toggle which installed actions the agent's LLM can see. Disabling does
	not uninstall — the action's manifest file stays in place and re-enabling
	restores it. Restart your MCP server after toggling so the LLM picks up
	the change.
</p>

{#if loading}
	<p class="text-muted-foreground">Loading installed actions…</p>
{:else if error}
	<p class="text-destructive" data-testid="actions-error">{error}</p>
{:else if actions.length === 0}
	<p class="text-muted-foreground">
		No actions installed. Run <code>aileron action add &lt;FQN&gt;</code> to
		install one.
	</p>
{:else}
	<section data-testid="installed-actions-section">
		<div class="flex flex-col gap-3">
			{#each actions as action (action.name)}
				{@const enabled = isEnabled(action)}
				<Card.Root data-testid="action-row" data-action-name={action.name}>
					<Card.Header>
						<div class="flex items-center justify-between">
							<div>
								<span class="font-semibold">{action.name}</span>
								<span class="ml-3 text-sm text-muted-foreground">v{action.version}</span>
							</div>
							<label class="flex cursor-pointer items-center gap-2 text-sm">
								<span class="text-muted-foreground">
									{enabled ? 'Enabled' : 'Disabled'}
								</span>
								<input
									type="checkbox"
									role="switch"
									aria-label="Enable {action.name}"
									data-testid="action-toggle"
									checked={enabled}
									disabled={toggling[action.name]}
									onchange={() => toggle(action)}
								/>
							</label>
						</div>
					</Card.Header>
					<Card.Content>
						<p class="text-xs text-muted-foreground break-all">{action.source}</p>
					</Card.Content>
				</Card.Root>
			{/each}
		</div>
	</section>
{/if}

{#if loadErrors.length > 0}
	<section class="mt-6" data-testid="load-errors-section">
		<h2 class="mb-2 text-sm font-semibold text-destructive">Load errors</h2>
		<ul class="space-y-1 text-xs text-muted-foreground">
			{#each loadErrors as e}
				<li>
					<code>{e.file}</code>: {e.message}
				</li>
			{/each}
		</ul>
	</section>
{/if}
