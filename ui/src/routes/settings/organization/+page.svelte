<script lang="ts">
	import { onMount } from 'svelte';
	import { getCurrentEnterprise, updateCurrentEnterprise } from '$lib/api';
	import { getUser } from '$lib/auth.svelte.js';
	import * as Card from '$lib/components/ui/card';
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
	</div>
{/if}
