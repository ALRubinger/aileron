<script lang="ts">
	import { onMount } from 'svelte';
	import { getUserLLMConfig, upsertUserLLMConfig, deleteUserLLMConfig } from '$lib/api';
	import * as Card from '$lib/components/ui/card';
	import * as Dialog from '$lib/components/ui/dialog';
	import { Input } from '$lib/components/ui/input';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';

	let config = $state<any>(null);
	let loading = $state(true);
	let saving = $state(false);
	let deleting = $state(false);
	let error = $state('');
	let success = $state('');
	let confirmDeleteOpen = $state(false);
	let showApiKey = $state(false);

	let provider = $state('anthropic');
	let modelResearch = $state('');
	let modelSynthesis = $state('');
	let apiKey = $state('');

	onMount(async () => {
		try {
			config = await getUserLLMConfig();
			provider = config.provider || 'anthropic';
			modelResearch = config.model_research || '';
			modelSynthesis = config.model_synthesis || '';
		} catch (e: any) {
			if (e.message?.includes('not found') || e.message?.includes('404')) {
				config = null;
			} else {
				error = e.message;
			}
		} finally {
			loading = false;
		}
	});

	async function handleSave() {
		error = '';
		success = '';

		if (!apiKey && !config) {
			error = 'API key is required';
			return;
		}
		if (!apiKey && config) {
			error = 'Enter a new API key to update your configuration';
			return;
		}

		saving = true;
		try {
			config = await upsertUserLLMConfig({
				provider,
				model_research: modelResearch,
				model_synthesis: modelSynthesis,
				api_key: apiKey
			});
			apiKey = '';
			showApiKey = false;
			success = 'LLM configuration saved';
			setTimeout(() => (success = ''), 3000);
		} catch (e: any) {
			error = e.message;
		} finally {
			saving = false;
		}
	}

	async function handleDelete() {
		error = '';
		success = '';
		deleting = true;
		try {
			await deleteUserLLMConfig();
			config = null;
			provider = 'anthropic';
			modelResearch = '';
			modelSynthesis = '';
			apiKey = '';
			confirmDeleteOpen = false;
			success = 'LLM configuration removed. Using server defaults.';
			setTimeout(() => (success = ''), 3000);
		} catch (e: any) {
			error = e.message;
		} finally {
			deleting = false;
		}
	}
</script>

<svelte:head>
	<title>LLM Provider - Aileron</title>
</svelte:head>

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else}
	<div class="flex flex-col gap-6">
		<Card.Root>
			<Card.Header>
				<div class="flex items-center justify-between">
					<div>
						<Card.Title>LLM Provider</Card.Title>
						<Card.Description>
							Configure your LLM provider, models, and API key for draft generation
						</Card.Description>
					</div>
					{#if config}
						<Badge variant="outline" class="text-green-600 border-green-500/50">Configured</Badge>
					{:else}
						<Badge variant="secondary">Using server defaults</Badge>
					{/if}
				</div>
			</Card.Header>
			<Card.Content>
				<div class="flex flex-col gap-4">
					<p class="text-xs text-muted-foreground">
						Your personal LLM configuration overrides organization and server defaults.
						Remove your configuration to fall back to organization or server defaults.
					</p>

					<div class="flex flex-col gap-1.5">
						<label for="llm_provider" class="text-sm font-medium">Provider</label>
						<select
							id="llm_provider"
							bind:value={provider}
							class="flex h-9 w-full max-w-sm rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
						>
							<option value="anthropic">Anthropic</option>
							<option value="openai" disabled>OpenAI (coming soon)</option>
						</select>
					</div>

					<div class="flex flex-col gap-1.5">
						<label for="llm_model_research" class="text-sm font-medium">Research model</label>
						<Input
							id="llm_model_research"
							bind:value={modelResearch}
							placeholder="e.g. claude-haiku-4-5-20251001"
							class="max-w-sm"
						/>
						<p class="text-xs text-muted-foreground">
							Fast model used for tool-based context gathering
						</p>
					</div>

					<div class="flex flex-col gap-1.5">
						<label for="llm_model_synthesis" class="text-sm font-medium">Synthesis model</label>
						<Input
							id="llm_model_synthesis"
							bind:value={modelSynthesis}
							placeholder="e.g. claude-sonnet-4-6"
							class="max-w-sm"
						/>
						<p class="text-xs text-muted-foreground">
							Capable model used for composing draft replies
						</p>
					</div>

					<div class="flex flex-col gap-1.5">
						<label for="llm_api_key" class="text-sm font-medium">API key</label>
						<div class="flex items-center gap-2 max-w-sm">
							<Input
								id="llm_api_key"
								type={showApiKey ? 'text' : 'password'}
								bind:value={apiKey}
								placeholder={config?.api_key_configured
									? 'Enter new key to update'
									: 'Enter your API key'}
								class="flex-1"
							/>
							<Button
								variant="ghost"
								size="sm"
								onclick={() => (showApiKey = !showApiKey)}
							>
								{showApiKey ? 'Hide' : 'Show'}
							</Button>
						</div>
						{#if config?.api_key_configured}
							<p class="text-xs text-muted-foreground">
								A key is already configured. Enter a new key to replace it.
							</p>
						{/if}
					</div>

					{#if error}
						<p class="text-sm text-destructive">{error}</p>
					{/if}
					{#if success}
						<p class="text-sm text-green-600">{success}</p>
					{/if}

					<div class="flex items-center gap-3">
						<Button onclick={handleSave} disabled={saving} size="sm">
							{saving ? 'Saving...' : 'Save'}
						</Button>
						{#if config}
							<Button
								variant="destructive"
								size="sm"
								onclick={() => (confirmDeleteOpen = true)}
							>
								Remove configuration
							</Button>
						{/if}
					</div>
				</div>
			</Card.Content>
		</Card.Root>
	</div>

	<Dialog.Root bind:open={confirmDeleteOpen}>
		<Dialog.Content>
			<Dialog.Header>
				<Dialog.Title>Remove LLM configuration?</Dialog.Title>
				<Dialog.Description>
					This will remove your personal LLM provider settings. Draft generation will
					fall back to your organization's configuration or the server defaults.
				</Dialog.Description>
			</Dialog.Header>
			<Dialog.Footer>
				<Button variant="outline" onclick={() => (confirmDeleteOpen = false)}>Cancel</Button>
				<Button variant="destructive" onclick={handleDelete} disabled={deleting}>
					{deleting ? 'Removing...' : 'Remove'}
				</Button>
			</Dialog.Footer>
		</Dialog.Content>
	</Dialog.Root>
{/if}
