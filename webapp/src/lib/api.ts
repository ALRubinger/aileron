// Local-webapp API client.
//
// The daemon serves this webapp at the same origin it serves the
// `/v1/*` API. No JWT, no token refresh — there's no multi-user auth
// boundary on the local surface. The one cross-cutting concern is
// vault-locked handling: under `aileron launch` the daemon may start
// vault-locked and refuse vault-needing endpoints with 423; the
// passphrase modal (#429) opens, the user unlocks, and the original
// request is retried.
//
// See ../../../ui/src/lib/api.ts for the cloud-tier reference client
// (frozen). The shape is deliberately pared-down here — only what
// the local launch UX needs.

import { onVaultLocked } from './vault.svelte';

const API_BASE = '';

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
	const res = await fetch(`${API_BASE}${path}`, {
		...init,
		headers: {
			'Content-Type': 'application/json',
			...(init?.headers ?? {})
		}
	});
	if (res.status === 423) {
		// Vault locked. Open the passphrase modal; on unlock, retry
		// the original request once. The modal is responsible for
		// resolving the promise we hand it via onVaultLocked.
		return onVaultLocked(() => apiFetch<T>(path, init));
	}
	if (res.status === 204) return null as T;
	if (!res.ok) {
		const body = await res
			.json()
			.catch(() => ({ error: { message: res.statusText } }));
		throw new Error(body?.error?.message || res.statusText);
	}
	return res.json();
}

// --- Vault unlock (#429) ---

export type LocalVaultStatus = {
	locked: boolean;
	state: 'missing' | 'locked' | 'unlocked';
};

/** Snapshot of the local vault's lock state. The webapp polls this on
 *  load so it can open the modal even if the user hasn't yet
 *  triggered a vault-needing call. */
export async function getLocalVaultStatus(): Promise<LocalVaultStatus> {
	const res = await fetch(`${API_BASE}/v1/vault/status`);
	if (!res.ok) {
		throw new Error(`vault status: ${res.statusText}`);
	}
	return res.json();
}

/** Submits the passphrase. Returns the new status on success. Throws
 *  with a recognisable message on 401 (wrong passphrase) so the modal
 *  can keep the field open and let the user retry. */
export async function unlockLocalVault(passphrase: string): Promise<LocalVaultStatus> {
	const res = await fetch(`${API_BASE}/v1/vault/unlock`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ passphrase })
	});
	if (res.status === 401) {
		throw new Error('wrong passphrase');
	}
	if (res.status === 404) {
		throw new Error('no vault file at the expected path');
	}
	if (!res.ok && res.status !== 409) {
		const body = await res
			.json()
			.catch(() => ({ error: { message: res.statusText } }));
		throw new Error(body?.error?.message || res.statusText);
	}
	return res.json();
}

// --- Action-level approvals (#418) ---
//
// The action-approval queue is the runtime-blocking shape: one entry
// per held-open `POST /v1/actions/{name}/run` waiting for a single
// yes/no decision. The agent surfaces the approval URL to the user via
// templated MCP tool descriptions; the user decides here.

/** Discriminates the user-facing card layout the webapp renders for a
 *  pending approval. Mirrors `PendingActionApprovalKind` in the Go
 *  codegen — keep the union in sync if the spec adds a new kind. */
export type PendingActionApprovalKind =
	| 'action'
	| 'comms_send'
	| 'comms_draft'
	| 'http_request';

export type PendingActionApproval = {
	id: string;
	kind: PendingActionApprovalKind;
	action_name: string;
	connector_fqn?: string;
	args?: Record<string, unknown>;
	session_id?: string;
	requested_at: string;
	preview?: ActionApprovalPreview;
};

/** Rendered output of the action manifest's `[approval.preview]`
 *  directive (ADR-0016). Surfaced on the approval card so the user
 *  sees an authoritative summary fetched from the connector at
 *  approval time, rather than agent-supplied hints. */
export type ActionApprovalPreview = {
	/** Rendered entries in the manifest's declared order. Omitted on
	 *  wholesale preview failure. */
	fields?: ActionApprovalPreviewField[];
	/** User-facing reason a wholesale preview failure occurred (e.g.
	 *  "preview unavailable: timeout"). Empty on success even when
	 *  some individual fields had missing paths. */
	unavailable?: string;
};

export type ActionApprovalPreviewField = {
	/** User-facing key from the manifest's `render` table. */
	label: string;
	/** Resolved string. Empty when `missing=true`; UI renders "n/a". */
	value?: string;
	/** True when the manifest's render path did not resolve. */
	missing?: boolean;
};

export type ActionApprovalListResponse = {
	items: PendingActionApproval[];
};

/** Returns the queue of pending action-level approvals. The webapp
 *  consumes this once on load (or on SSE-disconnect-driven re-fetch).
 *  Steady-state updates flow through `watchActionApprovals` instead. */
export async function listActionApprovals(): Promise<ActionApprovalListResponse> {
	return apiFetch('/v1/action-approvals');
}

/** Payload of the `resolved` SSE event. Carries the user's verdict so
 *  the webapp can drop the matching pending card without re-fetching
 *  the list. */
export type ResolvedActionApproval = {
	id: string;
	approved: boolean;
	reason?: string;
	decided_at: string;
};

/** Callbacks the SSE subscriber invokes on each event class. The page
 *  passes handlers that update its local state; the SSE wiring concerns
 *  itself only with parsing frames and reconnect. */
export type ActionApprovalSubscriber = {
	onSnapshot: (items: PendingActionApproval[]) => void;
	onPending: (item: PendingActionApproval) => void;
	onResolved: (resolved: ResolvedActionApproval) => void;
	onError?: (err: unknown) => void;
};

/** Opens an SSE connection to `/v1/action-approvals/watch` and forwards
 *  events to the supplied subscriber. Returns a close function the
 *  caller MUST invoke on unmount; calling it tears down the EventSource
 *  and stops dispatch.
 *
 *  Reconnect is delegated to the browser's built-in EventSource retry
 *  semantics (default 3s, configurable server-side via `retry:` lines).
 *  On a long-lived disconnect (the browser gives up), the page is
 *  expected to refetch via [listActionApprovals]. */
export function watchActionApprovals(sub: ActionApprovalSubscriber): () => void {
	const url = `${API_BASE}/v1/action-approvals/watch`;
	const es = new EventSource(url);

	es.addEventListener('snapshot', (e: MessageEvent) => {
		try {
			const payload = JSON.parse(e.data);
			sub.onSnapshot(payload.items ?? []);
		} catch (err) {
			sub.onError?.(err);
		}
	});
	es.addEventListener('pending', (e: MessageEvent) => {
		try {
			sub.onPending(JSON.parse(e.data));
		} catch (err) {
			sub.onError?.(err);
		}
	});
	es.addEventListener('resolved', (e: MessageEvent) => {
		try {
			sub.onResolved(JSON.parse(e.data));
		} catch (err) {
			sub.onError?.(err);
		}
	});
	es.addEventListener('error', (e) => {
		// EventSource auto-reconnects on transient failure. We only
		// surface the error once per disconnect — readyState === CLOSED
		// means the browser gave up; CONNECTING means it'll retry.
		if (es.readyState === EventSource.CLOSED) {
			sub.onError?.(e);
		}
	});

	return () => es.close();
}

/** Resolves a pending action-approval. The runtime's blocked
 *  `RunAction` unblocks on the next tick; on `approved=true` the action
 *  proceeds normally, on `approved=false` it returns an
 *  `approval_denied` failure envelope to the agent with `reason` in the
 *  message body. Returns null on success (server replies 204 No Content);
 *  throws on 404 (already resolved) or other non-2xx.
 *
 *  `editedPayload` carries kind-specific fields the user changed
 *  before approving — most prominently `{ body: "...new..." }` for
 *  `comms_draft` approvals (#428). The CommsServer's dispatcher
 *  reads this and sends the user-edited bytes rather than the
 *  agent's original draft. */
export async function decideActionApproval(
	approvalId: string,
	approved: boolean,
	reason?: string,
	editedPayload?: Record<string, unknown>
): Promise<null> {
	const body: Record<string, unknown> = { approved, reason: reason ?? '' };
	if (editedPayload && Object.keys(editedPayload).length > 0) {
		body.edited_payload = editedPayload;
	}
	return apiFetch(`/v1/action-approvals/${approvalId}/decide`, {
		method: 'POST',
		body: JSON.stringify(body)
	});
}

// --- Installed actions (ADR-0003) ---
//
// Mirrors `GET /v1/actions` and `PATCH /v1/actions/{name}`. The daemon
// merges the per-user overlay at `~/.aileron/action-state.json` into the
// response so the webapp sees one consistent view of installed actions
// plus their current enable/disable state.

export type InstalledAction = {
	name: string;
	version: string;
	source: string;
	/** Absent on older daemons; treat as `true` in that case. */
	enabled?: boolean;
};

export type InstalledActionList = {
	items?: InstalledAction[];
	load_errors?: Array<{
		class: string;
		message: string;
		file: string;
		line?: number;
		boundary?: string;
	}>;
};

/** Lists installed actions plus any per-file load errors. The response
 *  reflects the merged manifest+overlay view; `enabled=false` means the
 *  action is hidden from MCP `tools/list` until re-enabled. */
export async function listInstalledActions(): Promise<InstalledActionList> {
	return apiFetch('/v1/actions');
}

/** Toggles the `enabled` flag in the action's user-preference overlay.
 *  The manifest file is left untouched; only the overlay changes.
 *  Returns the daemon's updated view of the action so callers can
 *  reconcile their local state without a follow-up GET. */
export async function setActionEnabled(
	name: string,
	enabled: boolean
): Promise<InstalledAction> {
	return apiFetch(`/v1/actions/${encodeURIComponent(name)}`, {
		method: 'PATCH',
		body: JSON.stringify({ enabled })
	});
}

// --- Connector Hub (ADR-0013, #488) ---
//
// The daemon shallow-clones the public `aileron-connectors-hub` repo
// per query and serves discovery via /v1/hub/*. The webapp is one of
// two thin clients on top — the CLI (`aileron hub …`) is the other.
// The install modal layers on top of `/v1/hub/install-decision` and
// `/v1/connectors/install` to surface publisher trust before installing.

/** Single Hub connector entry. Mirrors `api.HubConnectorEntry` in the
 *  Go codegen — keep these shapes in sync with `internal/api/openapi.yaml`.
 *  Field names match the JSON wire format directly (snake_case). */
export type HubConnectorEntry = {
	fqn: string;
	description: string;
	publisher_github: string;
	key_url: string;
	release_pattern: string;
};

export type HubConnectorList = {
	connectors: HubConnectorEntry[];
};

/** Trust-state enum for the install-decision payload. Drives the
 *  modal's color and copy. `unknown` = first install from this
 *  publisher at this FQN; `already_trusted` = key already on the
 *  local keyring; `conflict` = the publisher's key differs from one
 *  the operator trusts for a sibling repo (rotation, MITM, or
 *  impersonation — surface in red). */
export type HubTrustState = 'already_trusted' | 'unknown' | 'conflict';

/** Pre-computed install-decision payload. Mirrors api.HubInstallDecision.
 *  The daemon fetches the publisher's current key, computes its
 *  fingerprint, and folds in local keyring state before responding,
 *  so the client renders the trust decision without ever holding key
 *  bytes. */
export type HubInstallDecision = {
	fqn: string;
	description: string;
	publisher_github: string;
	fingerprint: string;
	trust_state: HubTrustState;
	publisher_footprint: string[];
	risk_indicators: string[];
};

/** Lists every connector published to the Hub. Pass `q` to filter
 *  server-side (case-insensitive match on FQN and description). */
export async function listHubConnectors(q?: string): Promise<HubConnectorList> {
	const qs = q && q.trim() ? `?q=${encodeURIComponent(q.trim())}` : '';
	return apiFetch(`/v1/hub/connectors${qs}`);
}

/** Fetches the install-decision payload for a single FQN. Returns the
 *  same shape the CLI's `aileron connector install` renders. */
export async function getHubInstallDecision(fqn: string): Promise<HubInstallDecision> {
	return apiFetch(`/v1/hub/install-decision?fqn=${encodeURIComponent(fqn)}`);
}

/** Installed-connector envelope returned by POST /v1/connectors/install
 *  on success. `already_installed` is set when an offline-cached hash
 *  short-circuits the install pipeline (ADR-0004 §"Offline behavior"). */
export type InstalledConnector = {
	fqn: string;
	version: string;
	hash: string;
	entry_dir: string;
	already_installed?: boolean;
};

/** Runs the connector install pipeline. `confirmed_fingerprint` (per
 *  ADR-0013 / #487 Q4) triggers daemon-side trust persistence: the
 *  daemon verifies the publisher key's fingerprint matches what the
 *  webapp confirmed at the modal, then writes the key to the keyring
 *  under the FQN before running the install. Trust persists even if
 *  the install pipeline later fails. */
export async function installConnector(args: {
	fqn: string;
	version: string;
	confirmed_fingerprint?: string;
	expected_hash?: string;
}): Promise<InstalledConnector> {
	return apiFetch('/v1/connectors/install', {
		method: 'POST',
		body: JSON.stringify(args)
	});
}
