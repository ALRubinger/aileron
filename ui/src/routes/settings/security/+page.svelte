<script lang="ts">
	import * as Card from '$lib/components/ui/card';
</script>

<svelte:head>
	<title>Security - Aileron</title>
</svelte:head>

<div class="flex flex-col gap-6">
	<Card.Root>
		<Card.Header>
			<Card.Title>Vault encryption</Card.Title>
			<Card.Description>How your connected account credentials are protected</Card.Description>
		</Card.Header>
		<Card.Content>
			<div class="flex flex-col gap-4 text-sm">
				<div class="grid grid-cols-[auto_1fr] gap-x-8 gap-y-3">
					<span class="text-muted-foreground">Key derivation</span>
					<span>Argon2id (passphrase-derived key)</span>

					<span class="text-muted-foreground">Encryption</span>
					<span>AES-256-GCM envelope encryption</span>

					<span class="text-muted-foreground">Key location</span>
					<span>Derived in your browser</span>
				</div>

				<hr class="border-border" />

				<div class="flex flex-col gap-3 text-muted-foreground">
					<p>
						Your vault passphrase derives an encryption key (KEK) locally using
						Argon2id. That key encrypts your connected account credentials with
						AES-256-GCM before anything is stored.
					</p>
					<p>
						The passphrase and the derived key are computed in your browser. Aileron
						never sees your passphrase. If you forget it, connected accounts must be
						re-linked.
					</p>
				</div>
			</div>
		</Card.Content>
	</Card.Root>
</div>
