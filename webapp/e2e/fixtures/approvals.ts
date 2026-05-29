import { expect, test as base, type Page, type Request } from '@playwright/test';

/**
 * Mocked-API fixture for the /approvals page.
 *
 * The page consumes two endpoint families:
 *
 *   - GET /v1/action-approvals/watch  — Server-Sent Events feed of the
 *     pending queue (snapshot + pending + resolved frames). The page
 *     opens this via `new EventSource(...)` on mount.
 *   - POST /v1/action-approvals/{id}/decide  — user verdict. Plain
 *     fetch; returns 200 with an empty body on success.
 *
 * Playwright's `route.fulfill()` buffers the body, so streamed SSE is
 * not directly forwardable through `page.route()`. Instead, this
 * fixture overrides `window.EventSource` with a test-only class whose
 * frames are pushed from Node via `page.evaluate()`. The override is
 * installed via `addInitScript()` and so applies to every navigation in
 * the test's page context.
 *
 * The decide endpoint is mocked through `page.route()` and exposes the
 * captured request bodies for assertion. The decide-resolved
 * round-trip is decoupled: tests push the `resolved` SSE frame
 * themselves after asserting the decide POST, mirroring how the real
 * daemon broadcasts to all subscribers.
 */

import type {
	ActionApprovalPreview,
	ActionApprovalPreviewField,
	PendingActionApproval,
	ResolvedActionApproval
} from '../../src/lib/api';

const WATCH_URL_PATH = '/v1/action-approvals/watch';

/**
 * Bag of recorded decide-POST requests. Tests assert against this
 * (e.g. "the approve button POSTed with approved=true and the right id").
 * Cleared between tests by the fixture.
 */
export type DecideRecorder = {
	posts: Array<{ approvalId: string; body: DecideBody }>;
};

export type DecideBody = {
	approved: boolean;
	reason: string;
	edited_payload?: Record<string, unknown>;
};

/**
 * Live handle to the mocked SSE stream. Tests push frames; the page's
 * EventSource subscriber dispatches them as if they came from the
 * daemon.
 */
export type SSEController = {
	/**
	 * Replace the snapshot — useful when the test mounts the page and
	 * wants a non-empty initial state. Pushes an `event: snapshot`
	 * frame. Safe to call before navigation; the override buffers
	 * frames until the page constructs the EventSource.
	 */
	pushSnapshot: (items: PendingActionApproval[]) => Promise<void>;
	/** Push a `pending` frame — appears live in the page. */
	pushPending: (item: PendingActionApproval) => Promise<void>;
	/** Push a `resolved` frame — page removes the matching card. */
	pushResolved: (resolved: ResolvedActionApproval) => Promise<void>;
};

type ApprovalsFixtures = {
	approvalsMock: {
		sse: SSEController;
		decides: DecideRecorder;
	};
};

export const test = base.extend<ApprovalsFixtures>({
	approvalsMock: async ({ page }, use) => {
		await installEventSourceMock(page);
		const decides: DecideRecorder = { posts: [] };
		await installDecideMock(page, decides);

		const sse: SSEController = {
			pushSnapshot: (items) => emit(page, 'snapshot', { items }),
			pushPending: (item) => emit(page, 'pending', item),
			pushResolved: (resolved) => emit(page, 'resolved', resolved)
		};

		await use({ sse, decides });
	}
});

export { expect };

async function installEventSourceMock(page: Page): Promise<void> {
	await page.addInitScript(({ watchPath }) => {
		const inbox: Array<{ event: string; data: unknown }> = [];
		// One channel per EventSource — keyed by the resolved URL so a
		// test that navigates between pages doesn't cross frames.
		const channels = new Map<
			string,
			{
				dispatch: (event: string, data: unknown) => void;
				close: () => void;
			}
		>();

		(window as unknown as { __sseChannels__: typeof channels }).__sseChannels__ = channels;
		(window as unknown as { __sseInbox__: typeof inbox }).__sseInbox__ = inbox;

		class MockEventSource extends EventTarget {
			static readonly CONNECTING = 0;
			static readonly OPEN = 1;
			static readonly CLOSED = 2;
			readonly CONNECTING = 0;
			readonly OPEN = 1;
			readonly CLOSED = 2;
			readyState = 1;
			withCredentials = false;
			url: string;
			onopen: ((this: EventSource, ev: Event) => unknown) | null = null;
			onmessage: ((this: EventSource, ev: MessageEvent) => unknown) | null = null;
			onerror: ((this: EventSource, ev: Event) => unknown) | null = null;

			constructor(url: string | URL) {
				super();
				this.url =
					typeof url === 'string'
						? new URL(url, location.origin).pathname + new URL(url, location.origin).search
						: url.pathname + url.search;

				const dispatch = (event: string, data: unknown) => {
					if (this.readyState === 2) return;
					const ev = new MessageEvent(event, { data: JSON.stringify(data) });
					this.dispatchEvent(ev);
				};
				const close = () => {
					this.readyState = 2;
				};

				channels.set(this.url, { dispatch, close });

				// Drain any frames queued before the page constructed the
				// EventSource. The order of evaluate() vs onMount is not
				// strictly deterministic across runs, so this buffer keeps
				// pushSnapshot-before-navigate safe.
				if (this.url === watchPath) {
					queueMicrotask(() => {
						while (inbox.length > 0) {
							const f = inbox.shift();
							if (f) dispatch(f.event, f.data);
						}
					});
				}
			}

			close() {
				this.readyState = 2;
				channels.delete(this.url);
			}
		}

		// Replace the constructor in place. EventSource is a structural
		// type on window for our purposes — the page's api.ts only uses
		// addEventListener / close / readyState.
		(window as unknown as { EventSource: typeof MockEventSource }).EventSource = MockEventSource;
	}, { watchPath: WATCH_URL_PATH });
}

async function emit(page: Page, event: string, data: unknown): Promise<void> {
	await page.evaluate(
		({ event, data, watchPath }) => {
			const channels = (window as unknown as {
				__sseChannels__?: Map<
					string,
					{ dispatch: (e: string, d: unknown) => void }
				>;
			}).__sseChannels__;
			const inbox = (window as unknown as {
				__sseInbox__?: Array<{ event: string; data: unknown }>;
			}).__sseInbox__;
			const ch = channels?.get(watchPath);
			if (ch) {
				ch.dispatch(event, data);
			} else if (inbox) {
				// EventSource not yet constructed (test pushed snapshot
				// before navigation). Buffer for delivery on construct.
				inbox.push({ event, data });
			}
		},
		{ event, data, watchPath: WATCH_URL_PATH }
	);
}

async function installDecideMock(page: Page, decides: DecideRecorder): Promise<void> {
	// The local-daemon client performs a same-origin auth handshake before
	// API fetches and EventSource setup. Mocked E2E does not run a daemon,
	// so acknowledge the handshake without setting a cookie.
	await page.route('**/v1/auth/handshake', async (route) => {
		await route.fulfill({ status: 204 });
	});

	await page.route('**/v1/action-approvals/*/decide', async (route) => {
		const req: Request = route.request();
		const url = new URL(req.url());
		const segments = url.pathname.split('/');
		// path: /v1/action-approvals/{id}/decide
		const approvalId = segments[segments.length - 2];
		const body = (await safeJSON(req.postData())) as DecideBody;
		decides.posts.push({ approvalId, body });
		// Match the daemon's wire contract (#655): decide returns 204
		// No Content. apiFetch's 204 branch returns null cleanly;
		// returning 200 with an empty body would trip the json-parse
		// catch in the page and surface a spurious error.
		await route.fulfill({ status: 204 });
	});

	// The page also calls GET /v1/action-approvals on reconnect; mock
	// it as empty by default so a tab-level reload doesn't surprise the
	// test with a real-network attempt.
	await page.route('**/v1/action-approvals', async (route) => {
		await route.fulfill({
			status: 200,
			contentType: 'application/json',
			body: JSON.stringify({ items: [] })
		});
	});

	// The root layout calls /v1/vault/status on mount (vault.svelte.ts).
	// Mock it as `unlocked` so the passphrase modal stays closed; the
	// real daemon under `aileron launch` typically reports the same
	// state for an already-decrypted vault.
	await page.route('**/v1/vault/status', async (route) => {
		await route.fulfill({
			status: 200,
			contentType: 'application/json',
			body: JSON.stringify({ locked: false, state: 'unlocked' })
		});
	});
}

function safeJSON(s: string | null): Record<string, unknown> {
	if (!s) return {};
	try {
		return JSON.parse(s);
	} catch {
		return {};
	}
}

/**
 * Builders for the three in-scope kinds — keeps specs from re-asserting
 * the wire shape. Mirrors `internal/approval/action_queue.go:36-58` and
 * the openapi spec's `args` schema descriptions.
 */
export const fixture = {
	action: (overrides?: Partial<PendingActionApproval>): PendingActionApproval => ({
		id: 'act-action-1',
		kind: 'action',
		action_name: 'send-email',
		connector_fqn: 'github://aileron/aileron-connector-google',
		args: { to: 'alice@example.com', subject: 'hi' },
		session_id: 'session-42',
		requested_at: '2026-05-04T12:00:00Z',
		...overrides
	}),
	commsSend: (overrides?: Partial<PendingActionApproval>): PendingActionApproval => ({
		id: 'act-comms-send-1',
		kind: 'comms_send',
		action_name: 'send_message',
		args: {
			service: 'slack',
			channel: '#general',
			body: 'Deploying main to prod'
		},
		session_id: 'session-42',
		requested_at: '2026-05-04T12:01:00Z',
		...overrides
	}),
	httpRequest: (overrides?: Partial<PendingActionApproval>): PendingActionApproval => ({
		id: 'act-http-1',
		kind: 'http_request',
		action_name: 'http_request',
		args: {
			method: 'POST',
			url: 'https://api.example.com/widgets',
			body: '{"name":"new"}',
			secret_name: 'example_api_key'
		},
		session_id: 'session-42',
		requested_at: '2026-05-04T12:02:00Z',
		...overrides
	}),
	resolved: (id: string, approved: boolean, reason = ''): ResolvedActionApproval => ({
		id,
		approved,
		reason,
		decided_at: '2026-05-04T12:05:00Z'
	}),
	/**
	 * Build a {@link PendingActionApproval} for an action-kind entry
	 * that carries an `[approval.preview]` payload (ADR-0016). The
	 * default mirrors the worked example from the ADR (`send-draft`
	 * with To/Subject/Preview fields fetched authoritatively from
	 * Gmail).
	 */
	actionWithPreview: (
		overrides?: Partial<PendingActionApproval>,
		previewOverrides?: Partial<ActionApprovalPreview>
	): PendingActionApproval => ({
		id: 'act-preview-1',
		kind: 'action',
		action_name: 'send-draft',
		connector_fqn: 'github://aileron/aileron-connector-google',
		args: { draft_id: 'r-12345' },
		session_id: 'session-42',
		requested_at: '2026-05-04T12:00:00Z',
		preview: {
			fields: [
				{ label: 'To', value: 'alice@example.com' },
				{ label: 'Subject', value: 'Weekly recap' },
				{ label: 'Preview', value: "Here's the recap from this week's standup..." }
			],
			...previewOverrides
		},
		...overrides
	}),
	/**
	 * Convenience for the wholesale-failure shape (ADR-0016 failure
	 * modes table). The card renders the unavailable reason instead
	 * of the fields list.
	 */
	previewUnavailable: (reason: string): ActionApprovalPreview => ({
		unavailable: reason
	}),
	previewField: (
		label: string,
		opts?: { value?: string; missing?: boolean }
	): ActionApprovalPreviewField => ({
		label,
		...(opts?.value !== undefined ? { value: opts.value } : {}),
		...(opts?.missing ? { missing: true } : {})
	})
};
