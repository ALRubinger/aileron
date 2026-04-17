import { describe, it, expect } from 'vitest';
import {
	approvalStatusColor,
	riskColor,
	effectColor,
	connectedAccountStatusColor,
	eventColor
} from './colors';

describe('approvalStatusColor', () => {
	it('returns yellow for pending', () => {
		expect(approvalStatusColor('pending')).toBe('var(--color-status-yellow)');
	});

	it('returns green for approved', () => {
		expect(approvalStatusColor('approved')).toBe('var(--color-status-green)');
	});

	it('returns red for denied', () => {
		expect(approvalStatusColor('denied')).toBe('var(--color-status-red)');
	});

	it('returns orange for modified', () => {
		expect(approvalStatusColor('modified')).toBe('var(--color-status-orange)');
	});

	it('returns muted-foreground for unknown', () => {
		expect(approvalStatusColor('unknown')).toBe('var(--color-muted-foreground)');
	});
});

describe('riskColor', () => {
	it('returns green for low', () => {
		expect(riskColor('low')).toBe('var(--color-status-green)');
	});

	it('returns yellow for medium', () => {
		expect(riskColor('medium')).toBe('var(--color-status-yellow)');
	});

	it('returns orange for high', () => {
		expect(riskColor('high')).toBe('var(--color-status-orange)');
	});

	it('returns red for critical', () => {
		expect(riskColor('critical')).toBe('var(--color-status-red)');
	});

	it('returns muted-foreground for unknown', () => {
		expect(riskColor('unknown')).toBe('var(--color-muted-foreground)');
	});
});

describe('effectColor', () => {
	it('returns green for allow', () => {
		expect(effectColor('allow')).toBe('var(--color-status-green)');
	});

	it('returns red for deny', () => {
		expect(effectColor('deny')).toBe('var(--color-status-red)');
	});

	it('returns yellow for require_approval', () => {
		expect(effectColor('require_approval')).toBe('var(--color-status-yellow)');
	});

	it('returns orange for allow_with_modification', () => {
		expect(effectColor('allow_with_modification')).toBe('var(--color-status-orange)');
	});

	it('returns muted-foreground for unknown', () => {
		expect(effectColor('unknown')).toBe('var(--color-muted-foreground)');
	});
});

describe('connectedAccountStatusColor', () => {
	it('returns green for active', () => {
		expect(connectedAccountStatusColor('active')).toBe('var(--color-status-green)');
	});

	it('returns yellow for expired', () => {
		expect(connectedAccountStatusColor('expired')).toBe('var(--color-status-yellow)');
	});

	it('returns red for revoked', () => {
		expect(connectedAccountStatusColor('revoked')).toBe('var(--color-status-red)');
	});

	it('returns muted-foreground for unknown', () => {
		expect(connectedAccountStatusColor('unknown')).toBe('var(--color-muted-foreground)');
	});
});

describe('eventColor', () => {
	it('returns blue for submitted events', () => {
		expect(eventColor('request_submitted')).toBe('var(--color-status-blue)');
	});

	it('returns green for approved events', () => {
		expect(eventColor('request_approved')).toBe('var(--color-status-green)');
	});

	it('returns red for denied events', () => {
		expect(eventColor('request_denied')).toBe('var(--color-status-red)');
	});

	it('returns green for succeeded events', () => {
		expect(eventColor('execution_succeeded')).toBe('var(--color-status-green)');
	});

	it('returns red for failed events', () => {
		expect(eventColor('execution_failed')).toBe('var(--color-status-red)');
	});

	it('returns green for granted events', () => {
		expect(eventColor('access_granted')).toBe('var(--color-status-green)');
	});

	it('returns green for issued events', () => {
		expect(eventColor('token_issued')).toBe('var(--color-status-green)');
	});

	it('returns muted-foreground for unknown events', () => {
		expect(eventColor('something_else')).toBe('var(--color-muted-foreground)');
	});
});
