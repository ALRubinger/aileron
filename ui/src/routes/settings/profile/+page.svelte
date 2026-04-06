<script lang="ts">
	import { onMount } from 'svelte';
	import { getCurrentUser, updateCurrentUser, disconnectAuthProvider } from '$lib/api';
	import { refreshUser } from '$lib/auth.svelte.js';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { Separator } from '$lib/components/ui/separator';

	let user = $state<any>(null);
	let loading = $state(true);
	let saving = $state(false);
	let error = $state('');
	let success = $state('');
	let displayName = $state('');

	onMount(async () => {
		try {
			user = await getCurrentUser();
			displayName = user.display_name || '';
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	});

	async function handleSave() {
		error = '';
		success = '';
		saving = true;
		try {
			user = await updateCurrentUser({ display_name: displayName });
			await refreshUser();
			success = 'Profile updated';
			setTimeout(() => (success = ''), 3000);
		} catch (e: any) {
			error = e.message;
		} finally {
			saving = false;
		}
	}

	async function handleDisconnect(provider: string) {
		error = '';
		try {
			await disconnectAuthProvider(provider);
			user = await getCurrentUser();
		} catch (e: any) {
			error = e.message;
		}
	}

	function formatDate(d: string) {
		return new Date(d).toLocaleDateString(undefined, {
			year: 'numeric',
			month: 'long',
			day: 'numeric'
		});
	}

	function canDisconnect(provider: string): boolean {
		if (!user) return false;
		const oauthCount = user.auth_providers?.length ?? 0;
		if (provider === 'email') {
			// Can remove password if there's at least one OAuth provider.
			return oauthCount > 0;
		}
		// Can disconnect an OAuth provider if there's another provider or a password.
		return oauthCount > 1 || user.has_password;
	}
</script>

<svelte:head>
	<title>Profile - Aileron</title>
</svelte:head>

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else if !user}
	<p class="text-destructive">{error || 'Failed to load profile'}</p>
{:else}
	<div class="flex flex-col gap-6">
		<Card.Root>
			<Card.Header>
				<Card.Title>Profile</Card.Title>
				<Card.Description>Your personal account information</Card.Description>
			</Card.Header>
			<Card.Content>
				<div class="flex flex-col gap-4">
					{#if user.avatar_url}
						<div>
							<img
								src={user.avatar_url}
								alt="Avatar"
								class="w-16 h-16 rounded-full"
								referrerpolicy="no-referrer"
							/>
						</div>
					{/if}

					<div class="flex flex-col gap-1.5">
						<label for="display_name" class="text-sm font-medium">Display name</label>
						<Input id="display_name" bind:value={displayName} class="max-w-sm" />
					</div>

					<div class="flex flex-col gap-1.5">
						<span class="text-sm font-medium">Email</span>
						<span class="text-sm text-muted-foreground">{user.email}</span>
					</div>

					{#if error}
						<p class="text-sm text-destructive">{error}</p>
					{/if}
					{#if success}
						<p class="text-sm text-green-600">{success}</p>
					{/if}

					<div>
						<Button
							onclick={handleSave}
							disabled={saving || displayName === user.display_name}
							size="sm"
						>
							{saving ? 'Saving...' : 'Save'}
						</Button>
					</div>
				</div>
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Account details</Card.Title>
			</Card.Header>
			<Card.Content>
				<div class="grid grid-cols-[auto_1fr] gap-x-8 gap-y-3 text-sm">
					<span class="text-muted-foreground">Role</span>
					<span><Badge variant="outline">{user.role}</Badge></span>

					<span class="text-muted-foreground">Status</span>
					<span><Badge variant="outline">{user.status}</Badge></span>

					<span class="text-muted-foreground">Member since</span>
					<span>{formatDate(user.created_at)}</span>

					{#if user.last_login_at}
						<span class="text-muted-foreground">Last login</span>
						<span>{formatDate(user.last_login_at)}</span>
					{/if}
				</div>
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Connected providers</Card.Title>
				<Card.Description>Identity providers linked to your account</Card.Description>
			</Card.Header>
			<Card.Content>
				<div class="flex flex-col gap-3">
					{#if user.has_password}
						<div class="flex items-center justify-between">
							<div class="flex items-center gap-2">
								<Badge variant="outline">email</Badge>
								<span class="text-sm text-muted-foreground">Password login</span>
							</div>
						</div>
					{/if}

					{#each user.auth_providers ?? [] as provider}
						<div class="flex items-center justify-between">
							<div class="flex items-center gap-2">
								<Badge variant="outline">{provider.provider}</Badge>
								<span class="text-sm text-muted-foreground">
									Connected {formatDate(provider.connected_at)}
								</span>
							</div>
							{#if canDisconnect(provider.provider)}
								<Button
									size="sm"
									variant="ghost"
									onclick={() => handleDisconnect(provider.provider)}
								>
									Disconnect
								</Button>
							{/if}
						</div>
					{/each}

					{#if !user.has_password && (!user.auth_providers || user.auth_providers.length === 0)}
						<p class="text-sm text-muted-foreground">No providers connected.</p>
					{/if}
				</div>
			</Card.Content>
		</Card.Root>
	</div>
{/if}
