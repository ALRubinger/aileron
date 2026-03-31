<svelte:head>
	<title>Aileron</title>
</svelte:head>

<h1 style="font-size: 1.5rem; margin-bottom: 0.5rem;">Aileron</h1>
<p style="color: var(--text-muted);">Agentic control plane</p>

<div style="display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 1rem; margin-top: 2rem;">
	<a href="/approvals" style="display: block; padding: 1.5rem; border: 1px solid var(--border); border-radius: 8px; text-decoration: none; color: var(--text); background: var(--bg-card);">
		<div style="font-size: 1.1rem; font-weight: 600; margin-bottom: 0.5rem;">Approvals</div>
		<div style="font-size: 0.85rem; color: var(--text-muted);">Review and act on pending approval requests</div>
	</a>
	<a href="/traces" style="display: block; padding: 1.5rem; border: 1px solid var(--border); border-radius: 8px; text-decoration: none; color: var(--text); background: var(--bg-card);">
		<div style="font-size: 1.1rem; font-weight: 600; margin-bottom: 0.5rem;">Traces</div>
		<div style="font-size: 0.85rem; color: var(--text-muted);">Audit trail of all control plane events</div>
	</a>
	<a href="/policies" style="display: block; padding: 1.5rem; border: 1px solid var(--border); border-radius: 8px; text-decoration: none; color: var(--text); background: var(--bg-card);">
		<div style="font-size: 1.1rem; font-weight: 600; margin-bottom: 0.5rem;">Policies</div>
		<div style="font-size: 0.85rem; color: var(--text-muted);">View and manage policy rules</div>
	</a>
</div>
