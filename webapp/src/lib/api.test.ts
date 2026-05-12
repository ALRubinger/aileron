import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { decideActionApproval } from './api';

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
