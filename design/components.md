# Aileron Component Catalog

This document is the manifest of Aileron's pattern layer — components that sit above shadcn-svelte primitives and define the section-level shapes used across the docs site and webapp.

Read this when you need to know which pattern to use, or how to build a new one. Read `DESIGN.md` for the principles those components express. Read `tokens.json` for the underlying values.

## Format

Each entry follows the same shape:

- **Status**: `implemented` (in production), `scaffolded` (partial), or `proposed` (planned, not built)
- **Purpose**: One sentence on what the component does
- **When to use** / **When not to use**: Decision rules
- **Where it lives**: Paths in each stack
- **Props / Markers**: Public contract
- **Tokens consumed**: Which `tokens.json` values it reads
- **Composes**: shadcn-svelte primitives or other Aileron patterns it builds on
- **Accessibility notes**: A11y requirements

The catalog reflects what exists in the docs site today. The webapp's component layer is leaner and will be filled in as the webapp UI is designed.

---

## Section primitives

### `ProseSection` (auto-wrapped via rehype)

**Status**: implemented (docs)

**Purpose**: White card surface with `--shadow-low` lift that wraps each `h2` and its following content in the docs prose. The card chrome is generated at build time, not authored in markdown.

**Where it lives**:
- Generator: `docs/src/lib/rehype-section-wrapper.mjs`
- CSS: `docs/src/styles/global.css` → `.prose-section`
- Authors do not touch this — they write flat MDX.

**Markup emitted**: `<section class="prose-section">…</section>`

**Tokens consumed**: `color.mode.light.card`, `color.mode.light.card-foreground`, `radius.lg`, `elevation.low`, `spacing.rhythm.card-padding`

**Authoring controls**:
- `<SectionBreak />` — closes the current section. Content after this until the next `h2` renders unwrapped on the textured page bg.
- `<SectionResume />` — opens a new section without an `h2`. Useful after a `SectionBreak`, or before the first `h2` of a page.

Both markers are globally provided via `[...slug].astro`'s `<Content components={…} />`; authors don't need to import them in each MDX file.

### `Surface` / `Card` (shadcn-svelte Card)

**Status**: implemented (docs, webapp)

**Purpose**: Generic bounded surface for grouping content. White background, `ring-1 ring-foreground/10` hairline border, `--shadow-low` lift.

**Where it lives**:
- `docs/src/lib/components/ui/card/*.svelte` (shadcn-svelte components)
- `webapp/src/lib/components/ui/card/*.svelte` (same)

**Composes**: bits-ui primitives

**Props**: standard shadcn-svelte Card slots — `Card.Root`, `Card.Header`, `Card.Title`, `Card.Description`, `Card.Content`, `Card.Footer`, `Card.Action`

**Tokens consumed**: `color.mode.light.card`, `color.mode.light.card-foreground`, `radius.xl`, `elevation.low`

### `HighlightCard`

**Status**: implemented (docs)

**Purpose**: Prominent callout panel for in-page summaries, orientation notes, or expectations.

**Variants**:
- `primary` — `border-l-4 border-l-primary` accent on the left edge
- `muted` — `bg-muted` (the very light gray `oklch(0.97)`) instead of pure white card

**Where it lives**: `docs/src/lib/components/ui/highlight-card.svelte`

**Composes**: `Card.Root`, `Card.Header`, `Card.Title`, `Card.Content`

**Props**: `title?: string`, `variant?: 'primary' | 'muted'` (default `primary`), `class?: string`

### `Feature`

**Status**: implemented (docs)

**Purpose**: Small card with leading icon, title, and prose. Used in 3-up feature grids (e.g., "Determinism / Idempotency / Security").

**Where it lives**: `docs/src/lib/components/ui/feature.svelte`

**Composes**: `Card.Root` (size="sm"), `Card.Header`, `Card.Title`, `Card.Content`

**Props**: `icon: Component` (Lucide icon), `title: string`, `class?: string`

### `LiftoffCta`

**Status**: implemented (docs)

**Purpose**: Large call-to-action card pointing readers from the homepage intro into the Getting Started flow. Plane icon + heading + description + "Start" button with arrow.

**Where it lives**: `docs/src/lib/components/ui/liftoff-cta.svelte`

**Composes**: `HighlightCard` (variant="muted"), shadcn-svelte `Button`

**Tokens consumed**: `color.mode.light.primary`, `color.mode.light.primary-foreground`, `motion.duration.fast`

**Notes**: The Button uses `text-primary-foreground no-underline` explicitly because it renders as an `<a>` inside `.prose`, and prose link rules (lowered to `:where()` zero specificity inside `@layer base`) otherwise compete. Hover shrinks the button by 5% (`hover:scale-95`).

---

## Docs site infrastructure

### `Sidebar` (left nav)

**Status**: implemented (docs)

**Purpose**: Sticky left navigation. Drawer on mobile (off-canvas), in-flow at `lg+` (320px wide).

**Where it lives**: `docs/src/components/Sidebar.svelte`

**Structure**:
- Main navigation (scrollable, contains expandable section cards with white bg and `shadow-low`)
- References panel pinned to the bottom (CLI Reference, API Reference) with dark gray bg

**Tokens consumed**: `color.mode.light.muted` (sidebar bg), `color.mode.light.card` (section cards), `elevation.low`

### `TocAside` (right TOC)

**Status**: implemented (docs)

**Purpose**: Sticky right-side "On this page" navigation. Visible at `xl+` (1280px and up). Animates in and out (slide + fade + width) when the viewport crosses the breakpoint.

**Where it lives**: `docs/src/components/TableOfContents.svelte`, container styling in `docs/src/styles/global.css` → `.toc-aside`

**Tokens consumed**: `color.mode.light.muted`, `radius.md`, `elevation.low`, `motion.duration.slow`

**Notes**: The `.toc-aside` class uses raw CSS with explicit `transition: width 500ms, opacity 500ms, transform 500ms` and a media query at `1280px`. Tailwind responsive variants on the same element animated only in one direction; the explicit CSS works both ways. `min-width: 0` is required to override flexbox's implicit `min-width: auto`.

### `MarkdownForAI`

**Status**: implemented (docs)

**Purpose**: Top-right utility box exposing the page's raw markdown to AI tools. Three actions: Copy, View, Download.

**Where it lives**: `docs/src/components/MarkdownForAI.svelte`

**Tokens consumed**: `color.mode.light.muted` (box bg), `color.mode.light.card` (button bg), `elevation.low`

**Notes**: Buttons default white (`bg-card`) on the muted-gray panel bg. Hover state uses `bg-neutral-200` (slightly darker than `bg-muted`) so the hover reads as a distinct state.

### `ReferencesPanel`

**Status**: implemented (docs)

**Purpose**: Dark-gray panel at the bottom of the left sidebar holding CLI Reference and API Reference links. Inverted contrast against the rest of the sidebar.

**Where it lives**: inline in `docs/src/components/Sidebar.svelte` (second `<nav>` inside the sidebar)

**Tokens consumed**: Tailwind `bg-neutral-600` for the panel, `bg-neutral-500` for hover/active, `text-neutral-100`/`text-neutral-50` for text, `elevation.low`

**Notes**: Uses Tailwind's neutral palette directly rather than our semantic tokens because the inversion to dark requires a wider range than our light-mode semantic mappings provide. If we ever do dark mode treatment for these panels, this would move to a semantic token.

### `Footer`

**Status**: implemented (docs)

**Purpose**: Page footer with Privacy Policy and Terms of Use links. Light gray panel that spans the full width of the content area (under both prose column and TOC).

**Where it lives**: `docs/src/components/Footer.svelte`, positioned by `docs/src/layouts/DocsLayout.astro`

**Tokens consumed**: Tailwind `bg-neutral-200` (slightly darker than the sidebar's `bg-muted`)

### `BaseLayout` header

**Status**: implemented (docs)

**Purpose**: Fixed top navigation. Aileron brand wordmark + tagline + GitHub icon link with shake-on-hover animation.

**Where it lives**: `docs/src/layouts/BaseLayout.astro` (inline `<header>`)

**Composition**:
- `Aileron` in `font-extrabold text-3xl` (the Black weight at display size)
- `ControlPlane` tagline in `font-normal text-3xl` (same size, Regular weight)
- GitHub icon (Octicons mark), right-aligned via `ml-auto`, animates with `.github-icon-link` CSS keyframe on hover

**Tokens consumed**: `color.mode.light.background`, `color.mode.light.border`, `color.mode.light.foreground`, `color.mode.light.muted-foreground`

---

## Patterns to evaluate

Patterns from Block's `docs-site-kickstarter` that may be worth lifting into Aileron once a use case appears. None of these are in v1.

- **`Hero`** — full-bleed hero with background imagery. Marketing landing pages.
- **`BentoGrid` + `BentoItem`** — asymmetric feature showcase grid. Product pages.
- **`FeatureSection`** — heading + paragraph + supporting visual. Explanatory sections.
- **`BrandWall` / `LogoWall`** — responsive logo grid for showing integrations or partner services. Once connector branding lands.
- **`FAQSection`** — accordion-based FAQ. Could use the existing accordion treatment (`forceMount`-based animation).
- **`Checklist`** — numbered or checkmarked list with prose per item. Already used in "Liftoff Preflight" implementation; could be generalized.

Each candidate gets a full entry above when it ships.

---

## Effects and utilities

### `github-icon-shake`

**Status**: implemented (docs)

**Purpose**: Hover animation on the GitHub icon in the header. 500ms scale-pulse (1 → 1.1 → 1) combined with a wiggle rotation (0° → -5° → +5° → 0°). Ported from itshover's animated GitHub icon (which uses React/motion); ours is pure CSS keyframes.

**Class**: `.github-icon-link` (wrapping `<a>`), animates the inner `<g class="github-icon-inner">` of the SVG on `:hover`

**Where it lives**: `docs/src/styles/global.css` and `docs/src/layouts/BaseLayout.astro`
