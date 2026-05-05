// Local-webapp API client.
//
// The daemon serves this webapp at the same origin it serves the
// `/v1/*` API. No JWT, no token refresh, no vault-locked retry — the
// vault is unlocked at launch and stays unlocked for the session's
// lifetime, and there's no multi-user auth boundary on the local
// surface. If a request fails, the caller surfaces the error.
//
// See ../../../ui/src/lib/api.ts for the cloud-tier reference client
// (frozen). The shape is deliberately pared-down here — only what
// the action-approval flow needs.

const API_BASE = '';

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
	const res = await fetch(`${API_BASE}${path}`, {
		...init,
		headers: {
			'Content-Type': 'application/json',
			...(init?.headers ?? {})
		}
	});
	if (res.status === 204) return null as T;
	if (!res.ok) {
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

export type PendingActionApproval = {
	id: string;
	action_name: string;
	connector_fqn?: string;
	args?: Record<string, unknown>;
	session_id?: string;
	requested_at: string;
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
 *  message body. Returns null on success (server replies 200 with empty
 *  body); throws on 404 (already resolved) or other non-2xx. */
export async function decideActionApproval(
	approvalId: string,
	approved: boolean,
	reason?: string
): Promise<null> {
	return apiFetch(`/v1/action-approvals/${approvalId}/decide`, {
		method: 'POST',
		body: JSON.stringify({ approved, reason: reason ?? '' })
	});
}
