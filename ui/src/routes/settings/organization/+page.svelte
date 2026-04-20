<script lang="ts">
	import { onMount } from 'svelte';
	import {
		getCurrentEnterprise,
		updateCurrentEnterprise,
		getEnterpriseLLMConfig,
		upsertEnterpriseLLMConfig,
		deleteEnterpriseLLMConfig
	} from '$lib/api';
	import { getUser } from '$lib/auth.svelte.js';
	import * as Card from '$lib/components/ui/card';
	import * as Dialog from '$lib/components/ui/dialog';
	import { Input } from '$lib/components/ui/input';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';

	let enterprise = $state<any>(null);
	let loading = $state(true);
	let saving = $state(false);
	let error = $state('');
	let success = $state('');

	let name = $state('');
	let billingEmail = $state('');
	let ssoRequired = $state(false);
	let allowedProviders = $state('');
	let allowedDomains = $state('');

	// LLM provider configuration state.
	let llmConfig = $state<any>(null);
	let llmSaving = $state(false);
	let llmDeleting = $state(false);
	let llmError = $state('');
	let llmSuccess = $state('');
	let llmConfirmDeleteOpen = $state(false);
	let llmShowApiKey = $state(false);

	let llmProvider = $state('anthropic');
	let llmModelResearch = $state('');
	let llmModelSynthesis = $state('');
	let llmApiKey = $state('');

	$effect(() => {
		if (enterprise) {
			name = enterprise.name || '';
			billingEmail = enterprise.billing_email || '';
			ssoRequired = enterprise.sso_required || false;
			allowedProviders = (enterprise.allowed_auth_providers || []).join(', ');
			allowedDomains = (enterprise.allowed_email_domains || []).join(', ');
		}
	});

	const canEdit = $derived(
		(() => {
			const user = getUser();
			return user?.role === 'owner' || user?.role === 'admin';
		})()
	);

	onMount(async () => {
		try {
			enterprise = await getCurrentEnterprise();
			// Load org LLM config (admin only, 404 is expected).
			if (canEdit && enterprise) {
				try {
					llmConfig = await getEnterpriseLLMConfig(enterprise.id);
					llmProvider = llmConfig.provider || 'anthropic';
					llmModelResearch = llmConfig.model_research || '';
					llmModelSynthesis = llmConfig.model_synthesis || '';
				} catch {
					llmConfig = null;
				}
			}
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	});

	function parseList(s: string): string[] {
		return s
			.split(',')
			.map((v) => v.trim())
			.filter(Boolean);
	}

	async function handleSave() {
		error = '';
		success = '';
		saving = true;
		try {
			enterprise = await updateCurrentEnterprise({
				name,
				billing_email: billingEmail,
				sso_required: ssoRequired,
				allowed_auth_providers: parseList(allowedProviders),
				allowed_email_domains: parseList(allowedDomains)
			});
			success = 'Organization updated';
			setTimeout(() => (success = ''), 3000);
		} catch (e: any) {
			error = e.message;
		} finally {
			saving = false;
		}
	}

	async function handleLLMSave() {
		llmError = '';
		llmSuccess = '';

		if (!llmApiKey && !llmConfig) {
			llmError = 'API key is required';
			return;
		}
		if (!llmApiKey && llmConfig) {
			llmError = 'Enter a new API key to update the configuration';
			return;
		}

		llmSaving = true;
		try {
			llmConfig = await upsertEnterpriseLLMConfig(enterprise.id, {
				provider: llmProvider,
				model_research: llmModelResearch,
				model_synthesis: llmModelSynthesis,
				api_key: llmApiKey
			});
			llmApiKey = '';
			llmShowApiKey = false;
			llmSuccess = 'Organization LLM configuration saved';
			setTimeout(() => (llmSuccess = ''), 3000);
		} catch (e: any) {
			llmError = e.message;
		} finally {
			llmSaving = false;
		}
	}

	async function handleLLMDelete() {
		llmError = '';
		llmSuccess = '';
		llmDeleting = true;
		try {
			await deleteEnterpriseLLMConfig(enterprise.id);
			llmConfig = null;
			llmProvider = 'anthropic';
			llmModelResearch = '';
			llmModelSynthesis = '';
			llmApiKey = '';
			llmConfirmDeleteOpen = false;
			llmSuccess = 'Organization LLM configuration removed. Using server defaults.';
			setTimeout(() => (llmSuccess = ''), 3000);
		} catch (e: any) {
			llmError = e.message;
		} finally {
			llmDeleting = false;
		}
	}

	function formatDate(d: string) {
		return new Date(d).toLocaleDateString(undefined, {
			year: 'numeric',
			month: 'long',
			day: 'numeric'
		});
	}
</script>

<svelte:head>
	<title>Organization - Aileron</title>
</svelte:head>

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else if !enterprise}
	<p class="text-destructive">{error || 'Failed to load organization'}</p>
{:else}
	<div class="flex flex-col gap-6">
		<Card.Root>
			<Card.Header>
				<Card.Title>Organization</Card.Title>
				<Card.Description>
					{#if enterprise.personal}
						Personal account
					{:else}
						Team account
					{/if}
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<div class="flex flex-col gap-4">
					<div class="flex flex-col gap-1.5">
						<label for="org_name" class="text-sm font-medium">Name</label>
						{#if canEdit}
							<Input id="org_name" bind:value={name} class="max-w-sm" />
						{:else}
							<span class="text-sm text-muted-foreground">{enterprise.name}</span>
						{/if}
					</div>

					<div class="flex flex-col gap-1.5">
						<label for="billing_email" class="text-sm font-medium">Billing email</label>
						{#if canEdit}
							<Input
								id="billing_email"
								type="email"
								bind:value={billingEmail}
								class="max-w-sm"
							/>
						{:else}
							<span class="text-sm text-muted-foreground">{enterprise.billing_email}</span>
						{/if}
					</div>

					<div class="grid grid-cols-[auto_1fr] gap-x-8 gap-y-3 text-sm">
						<span class="text-muted-foreground">Plan</span>
						<span><Badge variant="outline">{enterprise.plan}</Badge></span>

						<span class="text-muted-foreground">Slug</span>
						<span class="font-mono">{enterprise.slug}</span>

						<span class="text-muted-foreground">Created</span>
						<span>{formatDate(enterprise.created_at)}</span>
					</div>
				</div>
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Authentication policies</Card.Title>
				<Card.Description>
					Control how members sign in to your organization
				</Card.Description>
			</Card.Header>
			<Card.Content>
				<div class="flex flex-col gap-4">
					{#if canEdit}
						<label class="flex items-center gap-3">
							<input
								type="checkbox"
								bind:checked={ssoRequired}
								class="h-4 w-4 rounded border-border"
							/>
							<div>
								<span class="text-sm font-medium">Require SSO</span>
								<p class="text-xs text-muted-foreground">
									When enabled, members must sign in through an SSO provider
								</p>
							</div>
						</label>

						<div class="flex flex-col gap-1.5">
							<label for="allowed_providers" class="text-sm font-medium"
								>Allowed auth providers</label
							>
							<Input
								id="allowed_providers"
								bind:value={allowedProviders}
								placeholder="e.g. google, okta (leave empty to allow all)"
								class="max-w-sm"
							/>
							<p class="text-xs text-muted-foreground">Comma-separated list of provider identifiers</p>
						</div>

						<div class="flex flex-col gap-1.5">
							<label for="allowed_domains" class="text-sm font-medium"
								>Allowed email domains</label
							>
							<Input
								id="allowed_domains"
								bind:value={allowedDomains}
								placeholder="e.g. example.com (leave empty to allow all)"
								class="max-w-sm"
							/>
							<p class="text-xs text-muted-foreground">
								Comma-separated list of allowed email domains
							</p>
						</div>

						{#if error}
							<p class="text-sm text-destructive">{error}</p>
						{/if}
						{#if success}
							<p class="text-sm text-green-600">{success}</p>
						{/if}

						<div>
							<Button onclick={handleSave} disabled={saving} size="sm">
								{saving ? 'Saving...' : 'Save'}
							</Button>
						</div>
					{:else}
						<div class="grid grid-cols-[auto_1fr] gap-x-8 gap-y-3 text-sm">
							<span class="text-muted-foreground">SSO required</span>
							<span>{enterprise.sso_required ? 'Yes' : 'No'}</span>

							<span class="text-muted-foreground">Allowed providers</span>
							<span>
								{#if enterprise.allowed_auth_providers?.length}
									{enterprise.allowed_auth_providers.join(', ')}
								{:else}
									All
								{/if}
							</span>

							<span class="text-muted-foreground">Allowed domains</span>
							<span>
								{#if enterprise.allowed_email_domains?.length}
									{enterprise.allowed_email_domains.join(', ')}
								{:else}
									All
								{/if}
							</span>
						</div>

						<p class="text-xs text-muted-foreground mt-2">
							Only owners and admins can modify authentication policies.
						</p>
					{/if}
				</div>
			</Card.Content>
		</Card.Root>
		{#if canEdit}
			<Card.Root>
				<Card.Header>
					<div class="flex items-center justify-between">
						<div>
							<Card.Title>LLM Provider</Card.Title>
							<Card.Description>
								Default LLM configuration for all organization members
							</Card.Description>
						</div>
						{#if llmConfig}
							<Badge variant="outline" class="text-green-600 border-green-500/50">Configured</Badge>
						{:else}
							<Badge variant="secondary">Using server defaults</Badge>
						{/if}
					</div>
				</Card.Header>
				<Card.Content>
					<div class="flex flex-col gap-4">
						<p class="text-xs text-muted-foreground">
							This applies to all members who haven't set their own LLM configuration.
							Individual members can override this in their personal settings.
						</p>

						<div class="flex flex-col gap-1.5">
							<label for="org_llm_provider" class="text-sm font-medium">Provider</label>
							<select
								id="org_llm_provider"
								bind:value={llmProvider}
								class="flex h-9 w-full max-w-sm rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
							>
								<option value="anthropic">Anthropic</option>
								<option value="openai" disabled>OpenAI (coming soon)</option>
							</select>
						</div>

						<div class="flex flex-col gap-1.5">
							<label for="org_llm_research" class="text-sm font-medium">Research model</label>
							<Input
								id="org_llm_research"
								bind:value={llmModelResearch}
								placeholder="e.g. claude-haiku-4-5-20251001"
								class="max-w-sm"
							/>
							<p class="text-xs text-muted-foreground">
								Fast model used for tool-based context gathering
							</p>
						</div>

						<div class="flex flex-col gap-1.5">
							<label for="org_llm_synthesis" class="text-sm font-medium">Synthesis model</label>
							<Input
								id="org_llm_synthesis"
								bind:value={llmModelSynthesis}
								placeholder="e.g. claude-sonnet-4-6"
								class="max-w-sm"
							/>
							<p class="text-xs text-muted-foreground">
								Capable model used for composing draft replies
							</p>
						</div>

						<div class="flex flex-col gap-1.5">
							<label for="org_llm_api_key" class="text-sm font-medium">API key</label>
							<div class="flex items-center gap-2 max-w-sm">
								<Input
									id="org_llm_api_key"
									type={llmShowApiKey ? 'text' : 'password'}
									bind:value={llmApiKey}
									placeholder={llmConfig?.api_key_configured
										? 'Enter new key to update'
										: 'Enter your API key'}
									class="flex-1"
								/>
								<Button
									variant="ghost"
									size="sm"
									onclick={() => (llmShowApiKey = !llmShowApiKey)}
								>
									{llmShowApiKey ? 'Hide' : 'Show'}
								</Button>
							</div>
							{#if llmConfig?.api_key_configured}
								<p class="text-xs text-muted-foreground">
									A key is already configured. Enter a new key to replace it.
								</p>
							{/if}
						</div>

						{#if llmError}
							<p class="text-sm text-destructive">{llmError}</p>
						{/if}
						{#if llmSuccess}
							<p class="text-sm text-green-600">{llmSuccess}</p>
						{/if}

						<div class="flex items-center gap-3">
							<Button onclick={handleLLMSave} disabled={llmSaving} size="sm">
								{llmSaving ? 'Saving...' : 'Save'}
							</Button>
							{#if llmConfig}
								<Button
									variant="destructive"
									size="sm"
									onclick={() => (llmConfirmDeleteOpen = true)}
								>
									Remove configuration
								</Button>
							{/if}
						</div>
					</div>
				</Card.Content>
			</Card.Root>

			<Dialog.Root bind:open={llmConfirmDeleteOpen}>
				<Dialog.Content>
					<Dialog.Header>
						<Dialog.Title>Remove organization LLM configuration?</Dialog.Title>
						<Dialog.Description>
							This will remove the organization's LLM provider settings. All members
							without personal configurations will fall back to server defaults.
						</Dialog.Description>
					</Dialog.Header>
					<Dialog.Footer>
						<Button variant="outline" onclick={() => (llmConfirmDeleteOpen = false)}>Cancel</Button>
						<Button variant="destructive" onclick={handleLLMDelete} disabled={llmDeleting}>
							{llmDeleting ? 'Removing...' : 'Remove'}
						</Button>
					</Dialog.Footer>
				</Dialog.Content>
			</Dialog.Root>
		{/if}
	</div>
{/if}
