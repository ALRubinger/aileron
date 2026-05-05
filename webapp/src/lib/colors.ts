/** Approval status colors */
export function approvalStatusColor(status: string): string {
	switch (status) {
		case 'pending': return 'var(--color-status-yellow)';
		case 'approved': return 'var(--color-status-green)';
		case 'denied': return 'var(--color-status-red)';
		case 'modified': return 'var(--color-status-orange)';
		default: return 'var(--color-muted-foreground)';
	}
}

/** Risk level colors */
export function riskColor(level: string): string {
	switch (level) {
		case 'low': return 'var(--color-status-green)';
		case 'medium': return 'var(--color-status-yellow)';
		case 'high': return 'var(--color-status-orange)';
		case 'critical': return 'var(--color-status-red)';
		default: return 'var(--color-muted-foreground)';
	}
}

/** Policy effect colors */
export function effectColor(effect: string): string {
	switch (effect) {
		case 'allow': return 'var(--color-status-green)';
		case 'deny': return 'var(--color-status-red)';
		case 'require_approval': return 'var(--color-status-yellow)';
		case 'allow_with_modification': return 'var(--color-status-orange)';
		default: return 'var(--color-muted-foreground)';
	}
}

/** Connected account status colors */
export function connectedAccountStatusColor(status: string): string {
	switch (status) {
		case 'active': return 'var(--color-status-green)';
		case 'expired': return 'var(--color-status-yellow)';
		case 'revoked': return 'var(--color-status-red)';
		default: return 'var(--color-muted-foreground)';
	}
}

/** Trace event type colors */
export function eventColor(eventType: string): string {
	if (eventType.includes('submitted')) return 'var(--color-status-blue)';
	if (eventType.includes('approved')) return 'var(--color-status-green)';
	if (eventType.includes('denied')) return 'var(--color-status-red)';
	if (eventType.includes('succeeded')) return 'var(--color-status-green)';
	if (eventType.includes('failed')) return 'var(--color-status-red)';
	if (eventType.includes('granted') || eventType.includes('issued')) return 'var(--color-status-green)';
	return 'var(--color-muted-foreground)';
}
