import { codeToHtml } from 'shiki';

/**
 * Render code to themed HTML using Astro's default Shiki theme
 * (github-dark), matching what markdown fences produce. Call at MDX
 * module scope so the work happens at build time and the resulting
 * HTML string can be passed as a prop into Svelte islands that would
 * otherwise emit unhighlighted <pre><code>.
 */
export function highlight(code: string, lang: string): Promise<string> {
  return codeToHtml(code, { lang, theme: 'github-dark' });
}
