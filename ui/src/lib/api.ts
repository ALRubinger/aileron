const API_BASE = 'http://localhost:8080';

async function apiFetch(path: string, options?: RequestInit) {
	const res = await fetch(`${API_BASE}${path}`, {
		headers: { 'Content-Type': 'application/json' },
		...options
	});
	if (res.status === 204) return null;
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: { message: res.statusText } }));
		throw new Error(err.error?.message || res.statusText);
	}
	return res.json();
}

export async function listApprovals(workspaceId = 'default') {
	return apiFetch(`/v1/approvals?workspace_id=${workspaceId}`);
}

export async function getApproval(approvalId: string) {
	return apiFetch(`/v1/approvals/${approvalId}`);
}

export async function approveRequest(approvalId: string, comment?: string) {
	return apiFetch(`/v1/approvals/${approvalId}/approve`, {
		method: 'POST',
		body: JSON.stringify({ comment })
	});
}

export async function denyRequest(approvalId: string, reason: string, comment?: string) {
	return apiFetch(`/v1/approvals/${approvalId}/deny`, {
		method: 'POST',
		body: JSON.stringify({ reason, comment })
	});
}

export async function getIntent(intentId: string) {
	return apiFetch(`/v1/intents/${intentId}`);
}

export async function listTraces(workspaceId = 'default') {
	return apiFetch(`/v1/traces?workspace_id=${workspaceId}`);
}

export async function listPolicies(workspaceId = 'default') {
	return apiFetch(`/v1/policies?workspace_id=${workspaceId}`);
}

export async function searchMarketplace(query?: string) {
	const qs = query ? `?q=${encodeURIComponent(query)}` : '';
	return apiFetch(`/v1/marketplace/servers${qs}`);
}

export async function installMarketplaceServer(registryId: string) {
	return apiFetch(`/v1/marketplace/servers/${encodeURIComponent(registryId)}/install`, {
		method: 'POST'
	});
}

export async function listMCPServers() {
	return apiFetch('/v1/mcp-servers');
}

export async function getMCPServer(id: string) {
	return apiFetch(`/v1/mcp-servers/${id}`);
}

export async function deleteMCPServer(id: string) {
	return apiFetch(`/v1/mcp-servers/${id}`, { method: 'DELETE' });
}

export async function setMCPServerCredential(id: string, envVarName: string, secretValue: string) {
	return apiFetch(`/v1/mcp-servers/${id}/credentials`, {
		method: 'POST',
		body: JSON.stringify({ env_var_name: envVarName, secret_value: secretValue })
	});
}
