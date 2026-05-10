// Latest released Aileron version, fetched from the GitHub API at
// build time. Imported by getting-started.mdx (and any future install
// surfaces) so download URLs and version-text in docs always reflect
// the most recent published release without a manual docs bump.
//
// On a successful release, the release workflow triggers a Railway
// redeploy of the docs site so this module re-evaluates against the
// fresh /releases/latest endpoint. See .github/workflows/release.yml.
//
// Top-level await runs once per build at module-load time. Astro's
// static output captures the resulting `latestRelease` constant into
// the rendered HTML — no runtime fetch hits browsers.

export type LatestRelease = {
	/** Bare version, e.g. "0.0.2" — matches goreleaser's name_template. */
	version: string;
	/** Full git tag, e.g. "v0.0.2" — matches the GitHub Release tag. */
	tag: string;
};

// Fallback used only when the GitHub API call fails at build time
// (rate limit, transient outage, no network on the build host). Bump
// this in tandem with each release as a belt-and-suspenders measure;
// in normal builds the live fetch overrides it.
const FALLBACK: LatestRelease = {
	version: "0.0.2",
	tag: "v0.0.2",
};

const API_URL = "https://api.github.com/repos/ALRubinger/aileron/releases/latest";
const TIMEOUT_MS = 10_000;

async function fetchLatest(): Promise<LatestRelease> {
	// One unauthenticated fetch per docs build is well inside GitHub's
	// 60 req/hr unauth limit. If the build cadence ever climbs (e.g.
	// per-PR preview deploys), pass a token via Authorization header.
	const headers: Record<string, string> = {
		Accept: "application/vnd.github+json",
		"User-Agent": "aileron-docs-build",
		"X-GitHub-Api-Version": "2022-11-28",
	};

	const controller = new AbortController();
	const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);
	try {
		const res = await fetch(API_URL, { headers, signal: controller.signal });
		if (!res.ok) {
			console.warn(
				`[release.ts] GitHub API ${res.status} ${res.statusText}; using fallback ${FALLBACK.tag}`,
			);
			return FALLBACK;
		}
		const data = (await res.json()) as { tag_name?: string };
		const tag = data.tag_name;
		if (!tag || typeof tag !== "string") {
			console.warn(`[release.ts] GitHub API missing tag_name; using fallback ${FALLBACK.tag}`);
			return FALLBACK;
		}
		const version = tag.startsWith("v") ? tag.slice(1) : tag;
		return { version, tag };
	} catch (err) {
		const message = err instanceof Error ? err.message : String(err);
		console.warn(`[release.ts] GitHub API fetch failed (${message}); using fallback ${FALLBACK.tag}`);
		return FALLBACK;
	} finally {
		clearTimeout(timer);
	}
}

export const latestRelease = await fetchLatest();
