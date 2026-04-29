<script lang="ts">
	import { onMount } from 'svelte';
	import { getTeeStatus } from '$lib/api';
	import * as Card from '$lib/components/ui/card';
	import { Badge } from '$lib/components/ui/badge';
	import * as Collapsible from '$lib/components/ui/collapsible';

	let teeStatus = $state<any>(null);
	let loading = $state(true);
	let error = $state('');
	let showRawClaims = $state(false);

	onMount(async () => {
		try {
			teeStatus = await getTeeStatus();
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	});

	function formatDateTime(dateStr: string): string {
		return new Date(dateStr).toLocaleString(undefined, {
			year: 'numeric',
			month: 'short',
			day: 'numeric',
			hour: '2-digit',
			minute: '2-digit',
			timeZoneName: 'short'
		});
	}

	function isExpired(dateStr: string): boolean {
		return new Date(dateStr) < new Date();
	}
</script>

<svelte:head>
	<title>Security - Aileron</title>
</svelte:head>

{#if loading}
	<p class="text-muted-foreground">Loading...</p>
{:else if error}
	<p class="text-destructive">{error}</p>
{:else}
	<div class="flex flex-col gap-6">
		<Card.Root>
			<Card.Header>
				<div class="flex items-center justify-between">
					<div>
						<Card.Title>TEE Status</Card.Title>
						<Card.Description>Trusted Execution Environment configuration and attestation</Card.Description>
					</div>
					{#if teeStatus.attested && teeStatus.provider === 'confidential-space'}
						<Badge variant="outline" class="text-green-600 border-green-500/50">
							Verified by Google Confidential Computing
						</Badge>
					{:else if teeStatus.attested && teeStatus.provider === 'local'}
						<Badge variant="secondary">Local dev mode</Badge>
					{:else if teeStatus.enabled}
						<Badge variant="secondary">Awaiting attestation</Badge>
					{:else}
						<Badge variant="secondary">Disabled</Badge>
					{/if}
				</div>
			</Card.Header>
			<Card.Content>
				<div class="flex flex-col gap-4">
					<div class="grid grid-cols-[auto_1fr] gap-x-8 gap-y-3 text-sm">
						<span class="text-muted-foreground">Provider</span>
						<span><Badge variant="outline">{teeStatus.provider}</Badge></span>

						<span class="text-muted-foreground">Enabled</span>
						<span>{teeStatus.enabled ? 'Yes' : 'No'}</span>

						<span class="text-muted-foreground">Attested</span>
						<span>{teeStatus.attested ? 'Yes' : 'No'}</span>
					</div>

					{#if teeStatus.attestation_claims}
						{@const claims = teeStatus.attestation_claims}
						<hr class="border-border" />

						<h4 class="text-sm font-medium">Attestation claims</h4>

						<div class="grid grid-cols-[auto_1fr] gap-x-8 gap-y-3 text-sm">
							{#if claims.image_digest}
								<span class="text-muted-foreground">Image digest</span>
								<code class="text-xs font-mono bg-muted px-1.5 py-0.5 rounded break-all">{claims.image_digest}</code>
							{/if}

							{#if claims.project_id}
								<span class="text-muted-foreground">Project ID</span>
								<span>{claims.project_id}</span>
							{/if}

							{#if claims.hwmodel}
								<span class="text-muted-foreground">Hardware model</span>
								<span>{claims.hwmodel}</span>
							{/if}

							{#if claims.issuer}
								<span class="text-muted-foreground">Token issuer</span>
								<span>{claims.issuer}</span>
							{/if}

							{#if claims.issued_at}
								<span class="text-muted-foreground">Attestation time</span>
								<span>{formatDateTime(claims.issued_at)}</span>
							{/if}

							{#if claims.expires_at}
								<span class="text-muted-foreground">Token expiry</span>
								<span>
									{formatDateTime(claims.expires_at)}
									{#if isExpired(claims.expires_at)}
										<Badge variant="destructive" class="ml-2 text-xs">Expired</Badge>
									{/if}
								</span>
							{/if}
						</div>

						<Collapsible.Root bind:open={showRawClaims}>
							<Collapsible.Trigger
								class="text-sm text-muted-foreground hover:text-foreground underline-offset-4 hover:underline transition-colors"
							>
								{showRawClaims ? 'Hide' : 'Show'} raw claims
							</Collapsible.Trigger>
							<Collapsible.Content>
								<pre class="mt-2 text-xs font-mono bg-muted p-3 rounded-md overflow-x-auto">{JSON.stringify(claims, null, 2)}</pre>
							</Collapsible.Content>
						</Collapsible.Root>
					{/if}

					{#if teeStatus.expected_identity}
						{@const expected = teeStatus.expected_identity}
						<hr class="border-border" />

						<h4 class="text-sm font-medium">Expected enclave identity</h4>

						<div class="grid grid-cols-[auto_1fr] gap-x-8 gap-y-3 text-sm">
							{#if expected.image_digest}
								<span class="text-muted-foreground">Image digest pin</span>
								<code class="text-xs font-mono bg-muted px-1.5 py-0.5 rounded break-all">{expected.image_digest}</code>
							{/if}

							{#if expected.project_id}
								<span class="text-muted-foreground">Project ID pin</span>
								<span>{expected.project_id}</span>
							{/if}
						</div>
					{/if}
				</div>
			</Card.Content>
		</Card.Root>

		<Card.Root>
			<Card.Header>
				<Card.Title>Trust model</Card.Title>
			</Card.Header>
			<Card.Content>
				<div class="flex flex-col gap-3 text-sm text-muted-foreground">
					<p>
						Attestation tokens are verified against Google's JWKS (JSON Web Key Set),
						fetched through a server-side proxy. The browser also pins the configured
						enclave image digest and project ID during vault unlock when those pins are
						available.
					</p>
					<p>
						The ECDH key exchange between your browser and the enclave remains the
						primary cryptographic protection. Even if the server serves tampered JWKS,
						it cannot decrypt your vault key material without access to the enclave's
						private key.
					</p>
				</div>
			</Card.Content>
		</Card.Root>
	</div>
{/if}
