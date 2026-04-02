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

/** Server status colors */
export function serverStatusColor(status: string): string {
	switch (status) {
		case 'running': return 'var(--color-status-green)';
		case 'error': return 'var(--color-status-red)';
		default: return 'var(--color-muted-foreground)';
	}
}

/** Server mode colors */
export function modeColor(mode: string): string {
	return mode === 'remote' ? 'var(--color-status-orange)' : 'var(--color-status-blue)';
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
