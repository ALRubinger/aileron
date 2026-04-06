import posthog from 'posthog-js';
import { PUBLIC_POSTHOG_KEY, PUBLIC_POSTHOG_HOST } from '$env/static/public';

export function initPosthog(): void {
	if (!PUBLIC_POSTHOG_KEY) {
		return;
	}
	posthog.init(PUBLIC_POSTHOG_KEY, {
		api_host: PUBLIC_POSTHOG_HOST || 'https://us.i.posthog.com',
		person_profiles: 'identified_only',
		capture_pageview: false,
		capture_pageleave: true
	});
}

export { posthog };
