import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
	decideActionApproval,
	getHubInstallDecision,
	listRecentMaterialized,
	getAuditByContentHash,
	getAuditByInvocation
} from './api';
import type { HubInstallDecision, AuditEvent } from './api';

// Regression target: the user clicked "Approve" in the webapp, the
// daemon executed the action successfully, but the page surfaced
// `Failed to execute 'json' on 'Response': Unexpected end of JSON input`.
// Root cause: the decide endpoint returned 200 OK with an empty body,
// and apiFetch unconditionally calls `res.json()` on any 2xx that
// isn't 204. The fix is to make the endpoint return 204 No Content —
// apiFetch already handles 204 by returning null.
//
// These tests pin that contract from the client side: a 204 response
// must resolve to null without trying to deserialize the body.

describe('decideActionApproval — server returns 204 No Content', () => {
	let fetchSpy: ReturnType<typeof vi.spyOn>;

	beforeEach(() => {
		fetchSpy = vi.spyOn(globalThis, 'fetch');
	});

	afterEach(() => {
		fetchSpy.mockRestore();
	});

	it('resolves to null on a 204 with empty body (does not call res.json)', async () => {
		const jsonSpy = vi.fn();
		fetchSpy.mockResolvedValue(
			new Response(null, { status: 204, statusText: 'No Content' })
		);
		// Wrap the Response so we can spot any accidental json() call.
		fetchSpy.mockImplementationOnce(async () => {
			const r = new Response(null, { status: 204 });
			const origJson = r.json.bind(r);
			r.json = async () => {
				jsonSpy();
				return origJson();
			};
			return r;
		});

		const result = await decideActionApproval('act-test-1', true);

		expect(result).toBeNull();
		expect(jsonSpy).not.toHaveBeenCalled();
	});

	it('throws "Unexpected end of JSON input" if the server reverts to 200 with empty body', async () => {
		// This test pins the *negative* side of the contract: it
		// documents why we moved off the 200-empty-body shape. If
		// someone reverts the server to 200 + empty body, the client
		// will throw — which is exactly what the user saw. Keeping
		// this test makes the failure mode explicit.
		fetchSpy.mockResolvedValue(
			new Response('', { status: 200, statusText: 'OK' })
		);

		await expect(decideActionApproval('act-test-1', true)).rejects.toThrow(
			/Unexpected end of JSON input/
		);
	});

	it('sends approved and reason in the request body', async () => {
		fetchSpy.mockResolvedValue(new Response(null, { status: 204 }));

		await decideActionApproval('act-test-1', false, 'wrong recipient');

		expect(fetchSpy).toHaveBeenCalledOnce();
		const [, init] = fetchSpy.mock.calls[0];
		expect(JSON.parse(init!.body as string)).toEqual({
			approved: false,
			reason: 'wrong recipient'
		});
	});

	it('attaches edited_payload only when provided', async () => {
		fetchSpy.mockResolvedValue(new Response(null, { status: 204 }));

		await decideActionApproval('act-test-1', true, undefined, { body: 'edited' });

		const [, init] = fetchSpy.mock.calls[0];
		expect(JSON.parse(init!.body as string)).toEqual({
			approved: true,
			reason: '',
			edited_payload: { body: 'edited' }
		});
	});

	it('throws on 404 with the server error message', async () => {
		fetchSpy.mockResolvedValue(
			new Response(
				JSON.stringify({ error: { message: 'approval id is unknown or already resolved' } }),
				{ status: 404, headers: { 'Content-Type': 'application/json' } }
			)
		);

		await expect(decideActionApproval('act-test-1', true)).rejects.toThrow(
			'approval id is unknown or already resolved'
		);
	});
});

// The webapp client is a hand-maintained mirror of the daemon's
// OpenAPI shape. The owner-level publisher-divergence work (#1419) added
// an optional `key_divergence` boolean to the install-decision payload.
// These tests pin that the field round-trips through the client and that
// the divergence signal is non-blocking (the call resolves normally with
// a 200 even when key_divergence is true).
describe('getHubInstallDecision — key_divergence mirror (#1419)', () => {
	let fetchSpy: ReturnType<typeof vi.spyOn>;

	beforeEach(() => {
		fetchSpy = vi.spyOn(globalThis, 'fetch');
	});

	afterEach(() => {
		fetchSpy.mockRestore();
	});

	it('round-trips key_divergence: true alongside a conflict trust_state on a 200', async () => {
		const payload: HubInstallDecision = {
			fqn: 'github://alice/a',
			description: 'A connector',
			publisher_github: 'alice',
			fingerprint: 'sha256:abc',
			trust_state: 'conflict',
			publisher_footprint: ['github://alice/b'],
			risk_indicators: [
				'Fetched signing key differs from the key you trust for this publisher (owner-level)'
			],
			key_divergence: true
		};
		fetchSpy.mockResolvedValue(
			new Response(JSON.stringify(payload), {
				status: 200,
				headers: { 'Content-Type': 'application/json' }
			})
		);

		const got = await getHubInstallDecision('github://alice/a');

		expect(got.key_divergence).toBe(true);
		expect(got.trust_state).toBe('conflict');
		expect(got.risk_indicators).toHaveLength(1);
		// The publisher footprint and fingerprint still come through — the
		// divergence does not strip the decision down.
		expect(got.publisher_footprint).toEqual(['github://alice/b']);
		expect(got.fingerprint).toBe('sha256:abc');
	});

	it('tolerates an absent key_divergence field (optional, additive)', async () => {
		const payload = {
			fqn: 'github://alice/a',
			description: 'A connector',
			publisher_github: 'alice',
			fingerprint: 'sha256:abc',
			trust_state: 'already_trusted',
			publisher_footprint: [],
			risk_indicators: []
		};
		fetchSpy.mockResolvedValue(
			new Response(JSON.stringify(payload), {
				status: 200,
				headers: { 'Content-Type': 'application/json' }
			})
		);

		const got = await getHubInstallDecision('github://alice/a');

		expect(got.key_divergence).toBeUndefined();
		expect(got.trust_state).toBe('already_trusted');
	});
});

// --- Audit provenance wrappers (#1891) ---
//
// The provenance view reads `GET /v1/audit` same-origin via `apiFetch`.
// These tests pin the URL each wrapper builds (with encoding), the
// unwrap of `AuditListResponse.events`, and the empty-result contract.
// The handshake is a cached module-level promise; because it is resolved
// by an earlier test's fetch, we locate the audit call by URL rather than
// by call index so ordering does not matter.

function auditCall(
	fetchSpy: ReturnType<typeof vi.spyOn>,
	needle: string
): string {
	const call = fetchSpy.mock.calls.find((args: unknown[]) =>
		String(args[0]).includes(needle)
	);
	expect(call, `expected a fetch to a URL containing ${needle}`).toBeTruthy();
	return String(call![0]);
}

function auditResponse(events: AuditEvent[]): Response {
	return new Response(JSON.stringify({ events }), {
		status: 200,
		headers: { 'Content-Type': 'application/json' }
	});
}

const materializedEvent: AuditEvent = {
	audit_id: 'a1',
	event_type: 'output.materialized',
	timestamp: '2026-07-04T00:00:00Z',
	payload: {
		'aileron.output.name': 'report.csv',
		'aileron.output.content_hash': 'sha256:deadbeef'
	}
};

const nonMaterializedEvent: AuditEvent = {
	audit_id: 'a2',
	event_type: 'flightplan.launch',
	timestamp: '2026-07-04T00:00:00Z',
	payload: { sourceInputBindings: {} }
};

describe('audit wrappers — URL construction and unwrap', () => {
	let fetchSpy: ReturnType<typeof vi.spyOn>;

	beforeEach(() => {
		fetchSpy = vi.spyOn(globalThis, 'fetch');
	});
	afterEach(() => {
		fetchSpy.mockRestore();
	});

	it('listRecentMaterialized requests /v1/audit?limit and keeps only records with an output content hash', async () => {
		fetchSpy.mockResolvedValue(
			auditResponse([materializedEvent, nonMaterializedEvent])
		);

		const got = await listRecentMaterialized(25);

		const url = auditCall(fetchSpy, '/v1/audit?limit=');
		expect(url).toContain('limit=25');
		expect(got).toHaveLength(1);
		expect(got[0].audit_id).toBe('a1');
	});

	it('getAuditByContentHash encodes the hash into the content_hash filter', async () => {
		fetchSpy.mockResolvedValue(auditResponse([materializedEvent]));

		const got = await getAuditByContentHash('sha256:dead beef');

		const url = auditCall(fetchSpy, 'content_hash=');
		expect(url).toContain('content_hash=sha256%3Adead%20beef');
		expect(got).toHaveLength(1);
	});

	it('getAuditByInvocation encodes the id into the invocation_id filter', async () => {
		fetchSpy.mockResolvedValue(
			auditResponse([materializedEvent, nonMaterializedEvent])
		);

		const got = await getAuditByInvocation('inv/42');

		const url = auditCall(fetchSpy, 'invocation_id=');
		expect(url).toContain('invocation_id=inv%2F42');
		// No client-side filtering on invocation: both records come back.
		expect(got).toHaveLength(2);
	});

	it('returns [] when the daemon reports no events', async () => {
		// A Response body can only be read once, so return a fresh Response
		// per fetch call rather than sharing one instance across awaits.
		fetchSpy.mockImplementation(async () => auditResponse([]));
		expect(await getAuditByContentHash('sha256:none')).toEqual([]);
		expect(await getAuditByInvocation('inv-none')).toEqual([]);
		expect(await listRecentMaterialized()).toEqual([]);
	});

	it('surfaces a non-2xx via apiFetch throwing', async () => {
		fetchSpy.mockResolvedValue(
			new Response(JSON.stringify({ error: { message: 'audit list failed' } }), {
				status: 500,
				headers: { 'Content-Type': 'application/json' }
			})
		);
		await expect(getAuditByContentHash('sha256:x')).rejects.toThrow(
			'audit list failed'
		);
	});
});
