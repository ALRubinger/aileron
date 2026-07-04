// Local-webapp API client.
//
// The daemon serves this webapp at the same origin it serves the
// `/v1/*` API. The local daemon uses a single same-origin handshake to
// set an HttpOnly daemon-token cookie; there is no multi-user login,
// JWT refresh, or organization selector. The other cross-cutting
// concern is vault-locked handling: under `aileron launch` the daemon
// may start vault-locked and refuse vault-needing endpoints with 423;
// the passphrase modal (#429) opens, the user unlocks, and the
// original request is retried.
//
// See ../../../ui/src/lib/api.ts for the cloud-tier reference client
// (frozen). The shape is deliberately pared-down here — only what
// the local launch UX needs.

import { onVaultLocked } from './vault.svelte';

const API_BASE = '';
let daemonHandshake: Promise<void> | null = null;

async function ensureDaemonHandshake(): Promise<void> {
	if (!daemonHandshake) {
		daemonHandshake = fetch(`${API_BASE}/v1/auth/handshake`).then((res) => {
			if (!res.ok && res.status !== 204) {
				throw new Error(`daemon auth handshake: ${res.statusText}`);
			}
		});
	}
	return daemonHandshake;
}

/** Cross-cutting options for {@link apiFetch} that are not part of the
 *  wire request. `allow404` makes a 404 resolve to `null` instead of
 *  throwing, so callers whose "not found" is a normal, non-error result
 *  (e.g. the audit trace endpoint's unknown-hash empty state) keep the
 *  centralized handshake + 423 vault-lock retry without special-casing
 *  the status themselves. All other non-ok statuses still throw. */
type ApiFetchOptions = {
	allow404?: boolean;
};

async function apiFetch<T>(
	path: string,
	init?: RequestInit,
	opts?: ApiFetchOptions
): Promise<T> {
	await ensureDaemonHandshake();
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
		return onVaultLocked(() => apiFetch<T>(path, init, opts));
	}
	if (res.status === 204) return null as T;
	if (res.status === 404 && opts?.allow404) return null as T;
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
	await ensureDaemonHandshake();
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
	await ensureDaemonHandshake();
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

// --- Audit provenance (#1891) ---
//
// The provenance view (`/audit`) reads `GET /v1/audit` same-origin via
// `apiFetch`. The wire shape is `AuditListResponse` = `{ events: [] }`
// where each `AuditEvent` carries a free-form `payload` map. The
// provenance keys we render (`aileron.output.*`, `aileron.step.*`,
// `aileron.plan.*`, `aileron.actor.*`, `aileron.invocation.id`) live
// flat inside that payload (see internal/flightplan/runtime/audit.go),
// so the webapp models `payload` as an untyped map and reads the keys
// through the typed getters in `$lib/audit/payload`. Filtering the wire
// shape into a strong type here would lie about what the daemon sends.

/** Actor reference on an audit event. Mirrors `ActorRef` in the spec;
 *  the daemon normalizes `aileron.actor.identity_label` /
 *  `aileron.actor.credential_binding` onto this object at ingest. */
export type AuditActorRef = {
	id: string;
	type: 'agent' | 'human' | 'service' | 'connector_runtime';
	display_name?: string;
	identity_label?: string;
	credential_binding?: string;
};

/** One audit-log entry. `payload` is deliberately untyped: the provenance
 *  keys are flat `aileron.*` strings inside it, read through the payload
 *  getters rather than a strongly-typed wire shape. */
export type AuditEvent = {
	audit_id: string;
	event_type: string;
	timestamp: string;
	actor?: AuditActorRef;
	payload: Record<string, unknown>;
};

type AuditListResponse = {
	events?: AuditEvent[];
};

/** The `output.materialized` discriminator (internal/model EventTypeOutputMaterialized). */
export const EVENT_OUTPUT_MATERIALIZED = 'output.materialized';

/** Lists recent materialized-output provenance records for the landing
 *  view. Fetches the most recent audit events and filters to those that
 *  carry `aileron.output.content_hash` in their payload — robust to the
 *  exact discriminator string, since only a materialized-output record
 *  carries that key. */
export async function listRecentMaterialized(limit = 100): Promise<AuditEvent[]> {
	const resp = await apiFetch<AuditListResponse>(`/v1/audit?limit=${encodeURIComponent(String(limit))}`);
	const events = resp.events ?? [];
	return events.filter(
		(e) => typeof e.payload?.['aileron.output.content_hash'] === 'string'
	);
}

/** Resolves the audit event(s) for a single artifact content hash. The
 *  daemon supports the `content_hash` filter on `GET /v1/audit`. */
export async function getAuditByContentHash(hash: string): Promise<AuditEvent[]> {
	const resp = await apiFetch<AuditListResponse>(
		`/v1/audit?content_hash=${encodeURIComponent(hash)}`
	);
	return resp.events ?? [];
}

/** Resolves every audit record correlated by a launch invocation id.
 *  The daemon supports the `invocation_id` filter (#1893/#1907). */
export async function getAuditByInvocation(id: string): Promise<AuditEvent[]> {
	const resp = await apiFetch<AuditListResponse>(
		`/v1/audit?invocation_id=${encodeURIComponent(id)}`
	);
	return resp.events ?? [];
}

// --- Server-assembled provenance trace (#1913/#1930) ---
//
// `GET /v1/audit/trace` assembles the provenance graph server-side: the
// same walk-back the webapp used to do client-side (N+1 fan-out over
// `content_hash`) collapsed into one round-trip. The `/audit` graph view
// maps this response into the render model (`$lib/audit/provenance`); the
// timeline stays a flat list via `getAuditByInvocation`. Field names and
// node kinds mirror the client model, so this is a thin snake→camel map.

/** One directed edge in a server-assembled provenance graph, running
 *  parent → child (downstream consumer → upstream producer). Identical
 *  in shape to `ProvenanceEdge`. Mirrors `AuditTraceEdge` in the spec. */
export type AuditTraceEdge = {
	from: string;
	to: string;
};

/** One node in a server-assembled provenance graph. Mirrors
 *  `AuditTraceNode` in the spec — the wire shape differs from the render
 *  model (`ProvenanceNode`) only by `content_hash` being snake_case. */
export type AuditTraceNode = {
	id: string;
	kind: 'artifact' | 'step' | 'literal' | 'launch';
	title: string;
	subtitle?: string;
	depth: number;
	/** For an artifact node, the content hash it materialized. */
	content_hash?: string;
	/** True when an artifact node's hash resolved to no producing record. */
	dangling?: boolean;
	/** The backing audit event, when the node was derived from one. */
	event?: AuditEvent;
	/** For a literal node, the input binding/source it stands for. */
	literal?: { binding: string; source?: string };
};

/** Server-assembled provenance graph. Mirrors `AuditTraceResponse`.
 *  `root_id` is the id of the node the walk started at. */
export type AuditTraceResponse = {
	root_id: string;
	nodes: AuditTraceNode[];
	edges: AuditTraceEdge[];
};

/** Fetches the server-assembled provenance trace rooted at a content
 *  hash (precedence) or a launch invocation id. Returns `null` when the
 *  daemon reports 404 (unknown hash / invocation) so the caller renders
 *  the friendly empty state (#1894 contract) rather than treating a
 *  not-found as an error. Every other non-2xx still throws. */
export async function getAuditTrace(opts: {
	contentHash?: string;
	invocationId?: string;
}): Promise<AuditTraceResponse | null> {
	if (!opts.contentHash && !opts.invocationId) {
		throw new Error('getAuditTrace requires contentHash or invocationId');
	}
	const query = opts.contentHash
		? `content_hash=${encodeURIComponent(opts.contentHash)}`
		: `invocation_id=${encodeURIComponent(opts.invocationId ?? '')}`;
	return apiFetch<AuditTraceResponse>(`/v1/audit/trace?${query}`, undefined, {
		allow404: true
	});
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
	/** Rendered projection of the gated action's call-time args through
	 *  the action manifest's `[[inputs]]` declarations (per the ADR-0003
	 *  amendment introducing per-input `label` and `multiline` keys).
	 *  The approval card renders these as labeled rows so the user reads
	 *  "To: alr@…", "Subject: …", multiline "Body: …" rather than a
	 *  raw JSON args dump. Omitted when the gated action has no
	 *  resolvable manifest or its manifest declares no inputs; in that
	 *  case the webapp falls back to the historic raw-JSON accordion. */
	input_fields?: ActionApprovalPreviewField[];
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
	/** True when the manifest's `multiline` directive named this label.
	 *  The approval card renders these as scrollable blockquotes below
	 *  the inline rows rather than as single-line key/value entries. */
	multiline?: boolean;
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
	let es: EventSource | null = null;
	let closed = false;

	void ensureDaemonHandshake()
		.then(() => {
			if (closed) return;
			es = new EventSource(url);

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
				if (es?.readyState === EventSource.CLOSED) {
					sub.onError?.(e);
				}
			});
		})
		.catch((err) => sub.onError?.(err));

	return () => {
		closed = true;
		es?.close();
	};
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
// The daemon shallow-clones the public `aileron-hub` repo
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

/** Trust-state enum for the install-decision payload, evaluated at
 *  owner granularity (ADR-0013 per-publisher trust). Drives the modal's
 *  color and copy. `unknown` = no owner-level grant for this publisher;
 *  `already_trusted` = an owner-level grant matches the fetched key (one
 *  grant covers every repo the owner publishes); `conflict` = an owner
 *  grant exists but the fetched key diverges from it (rotation, MITM, or
 *  impersonation — surface in red; informational, never blocks). */
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
	/** True when the fetched signing key diverges from the owner-level
	 *  keyring grant for this publisher. Informational only — never
	 *  blocks the install; also surfaced via risk_indicators and
	 *  trust_state: 'conflict'. Optional (additive, may be absent). */
	key_divergence?: boolean;
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

// --- Hub action and suite catalogs (#709, action-first amendment) ---
//
// The Hub catalog holds three entry types: connectors (above), actions
// (template pointers), and suites (bundles of actions). The webapp
// browses all three through the daemon's `/v1/hub/*` endpoints.

/** Single Hub action entry. Mirrors api.HubActionEntry. */
export type HubActionEntry = {
	fqn: string;
	description: string;
	publisher_github: string;
	connector_fqn: string;
	intents?: string[];
	category?: string;
};

export type HubActionList = {
	actions: HubActionEntry[];
};

/** Single Hub suite entry. Mirrors api.HubSuiteEntry. */
export type HubSuiteEntry = {
	fqn: string;
	description: string;
	publisher_github: string;
	member_actions: string[];
	connectors_required?: string[];
	category?: string;
};

export type HubSuiteList = {
	suites: HubSuiteEntry[];
};

/** Lists every action published to the Hub. Pass `q` to filter
 *  server-side. */
export async function listHubActions(q?: string): Promise<HubActionList> {
	const qs = q && q.trim() ? `?q=${encodeURIComponent(q.trim())}` : '';
	return apiFetch(`/v1/hub/actions${qs}`);
}

/** Lists every suite published to the Hub. Pass `q` to filter
 *  server-side. */
export async function listHubSuites(q?: string): Promise<HubSuiteList> {
	const qs = q && q.trim() ? `?q=${encodeURIComponent(q.trim())}` : '';
	return apiFetch(`/v1/hub/suites${qs}`);
}

/** Single action entry by FQN. */
export async function getHubAction(fqn: string): Promise<HubActionEntry> {
	return apiFetch(`/v1/hub/action?fqn=${encodeURIComponent(fqn)}`);
}

/** Single suite entry by FQN. */
export async function getHubSuite(fqn: string): Promise<HubSuiteEntry> {
	return apiFetch(`/v1/hub/suite?fqn=${encodeURIComponent(fqn)}`);
}

/** Per-connector trust panel inside a composite install-decision.
 *  Shape mirrors api.HubInstallAuthority — same fields as the
 *  connector-level HubInstallDecision minus the top-level description. */
export type HubInstallAuthority = {
	fqn: string;
	publisher_github: string;
	fingerprint: string;
	trust_state: HubTrustState;
	publisher_footprint: string[];
	risk_indicators: string[];
	/** True when the fetched signing key diverges from the owner-level
	 *  keyring grant for this publisher. Informational only — never
	 *  blocks the install; also surfaced via risk_indicators and
	 *  trust_state: 'conflict'. Optional (additive, may be absent). */
	key_divergence?: boolean;
};

/** Composite install-decision payload for an action install.
 *  `authorities[]` always carries exactly one entry — the action's
 *  declared connector dependency. The `kind` discriminator lets the
 *  install modal render the action's own metadata alongside the
 *  underlying connector trust gate.
 *
 *  `latest_version` is the daemon-resolved SemVer tag the webapp uses
 *  as the install's `version` argument. Empty/omitted when the source
 *  has no published releases — in that case the modal disables the
 *  Install button and the operator falls back to the CLI. */
export type HubActionInstallDecision = {
	kind: 'action';
	fqn: string;
	description: string;
	publisher_github: string;
	connector_fqn: string;
	authorities: HubInstallAuthority[];
	latest_version?: string;
};

/** Composite install-decision payload for a suite install.
 *  `authorities[]` carries one entry per unique connector authority in
 *  the suite's dependency closure. `latest_version` covers every
 *  member action — they all ship from the same source repo. */
export type HubSuiteInstallDecision = {
	kind: 'suite';
	fqn: string;
	description: string;
	publisher_github: string;
	member_actions: string[];
	authorities: HubInstallAuthority[];
	latest_version?: string;
};

/** Composite install-decision for an action FQN. */
export async function getHubActionInstallDecision(fqn: string): Promise<HubActionInstallDecision> {
	return apiFetch(`/v1/hub/action-install-decision?fqn=${encodeURIComponent(fqn)}`);
}

/** Composite install-decision for a suite FQN. */
export async function getHubSuiteInstallDecision(fqn: string): Promise<HubSuiteInstallDecision> {
	return apiFetch(`/v1/hub/suite-install-decision?fqn=${encodeURIComponent(fqn)}`);
}

/** One unbound credential capability surfaced on an install
 *  response. Mirrors api.UnboundCapability. */
export type UnboundCapability = {
	connector_fqn: string;
	kind: string;
	scope?: string;
};

/** One stale-binding transition surfaced on an install response per
 *  #741. The webapp's stale-binding panel renders these and offers an
 *  in-browser "Reauthorize" button per entry. */
export type StaleBinding = {
	name: string;
	connector_fqn: string;
	stale_reason: 'scope_drift' | 'no_grant_record';
	missing_scopes?: string[];
	service?: string;
};

/** Installed-connector envelope returned by POST /v1/connectors/install
 *  on success. `already_installed` is set when an offline-cached hash
 *  short-circuits the install pipeline (ADR-0004 §"Offline behavior").
 *  `newly_stale_bindings` carries bindings the scope-drift hook flipped
 *  to `stale` during this install (#741). */
export type InstalledConnector = {
	fqn: string;
	version: string;
	hash: string;
	entry_dir: string;
	already_installed?: boolean;
	newly_stale_bindings?: StaleBinding[];
};

/** Installed-action envelope returned by POST /v1/actions/install on
 *  success. Mirrors api.InstalledAction (the install-response shape —
 *  not the `/v1/actions` list shape, which is `ActionListEntry` in
 *  this file). `unbound_capabilities` and `newly_stale_bindings` are
 *  the install-time UX signals (#741) the webapp install modal
 *  surfaces on the success panel. */
export type InstalledActionResult = {
	name: string;
	fqn: string;
	version: string;
	source: string;
	path: string;
	already_installed?: boolean;
	unbound_capabilities?: UnboundCapability[];
	newly_stale_bindings?: StaleBinding[];
};

/** Per-authority trust confirmation supplied alongside an action
 *  install (#743). One entry per connector authority in the action's
 *  dependency closure — for a suite install, this is the suite's full
 *  set on every per-action call. The daemon iterates and writes trust
 *  to the keyring before running the install pipeline. */
export type ConfirmedFingerprint = {
	fqn: string;
	fingerprint: string;
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

/** Runs the action install pipeline (#743 webapp execution).
 *  `confirmed_fingerprints` carries one entry per connector authority
 *  in the action's dependency closure — the daemon verifies and writes
 *  trust to the keyring for each before running the install. The
 *  pipeline auto-installs connector dependencies when
 *  `auto_install_connectors: true` is set (typical for webapp installs;
 *  the webapp can't realistically run two preflight rounds). */
export async function installAction(args: {
	fqn: string;
	version: string;
	auto_install_connectors?: boolean;
	confirmed_fingerprints?: ConfirmedFingerprint[];
	force?: boolean;
}): Promise<InstalledActionResult> {
	return apiFetch('/v1/actions/install', {
		method: 'POST',
		body: JSON.stringify(args)
	});
}

/** Response from POST /v1/bindings/setup/oauth2/init. The caller
 *  directs the browser to `authorize_url`; the daemon's callback
 *  endpoint (#745) handles the rest when caller=daemon. */
export type OAuth2InitResponse = {
	session_id: string;
	authorize_url: string;
	redirect_uri: string;
};

/** Starts a reauthorize flow for an existing binding from the
 *  webapp. Caller=daemon means the daemon's own listener at
 *  /v1/bindings/setup/oauth2/callback receives the OAuth provider's
 *  redirect (the webapp can't `bind()` a TCP port); after the daemon
 *  completes the token exchange it 303s the browser to `return_to`
 *  with a `#aileron_oauth=ok&binding=<name>` fragment.
 *
 *  The caller passes the binding's identifying tuple (connector FQN,
 *  service, identity) — mirrors what `aileron binding reauthorize`
 *  derives from a bare binding name on the CLI side. */
export async function startReauthorize(args: {
	connector_fqn: string;
	identity: string;
	service?: string;
	return_to: string;
}): Promise<OAuth2InitResponse> {
	return apiFetch('/v1/bindings/setup/oauth2/init', {
		method: 'POST',
		body: JSON.stringify({
			connector_fqn: args.connector_fqn,
			identity: args.identity,
			service: args.service,
			purpose: 'reauthorize',
			caller: 'daemon',
			return_to: args.return_to
		})
	});
}
