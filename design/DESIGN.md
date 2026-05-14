# Aileron Design Specification

This document is the narrative spec for Aileron's visual design. It pairs with `tokens.json` (machine-readable values) and `components.md` (component catalog). Together these three artifacts define how Aileron looks across the documentation site and the webapp.

Read this file when you need the intent behind a design choice. Read `tokens.json` when you need a value. Read `components.md` when you need to know which component to use or how to build a new one.

## How this spec is used

The spec is authoritative. Implementation must follow it. Drift between docs and webapp is a bug.

The pipeline is:

```
design/tokens.json
    │
    ▼ design/build/generate-tokens.mjs
    │
    ▼ design/build/output/tokens.generated.css
    │
    ▼ (imported by docs/src/styles/global.css and webapp/src/app.css)
    │
shadcn-svelte components → Aileron pattern layer → pages
```

`tokens.json` is the single source of truth for values. The generator produces a CSS file that both stacks import. The `@theme inline` block in each stack maps those CSS variables onto Tailwind utilities. shadcn-svelte primitives consume the utilities. Aileron's higher-level pattern components compose the primitives.

When you change a value, change it in `tokens.json` and re-run the generator. Never hand-edit the generated CSS. Never inline raw values in components.

## Brand and voice

Aileron is a tool for engineers. The design must feel built, not branded. Visual decisions reflect that posture.

Three principles shape every choice:

**Confident.** Heavy display type and strong contrast carry the page. Aileron Black at large sizes does the work that color would do elsewhere.

**Restrained.** White space is generous. Density is reserved for code and tables. The eye should rest more than it scans.

**Functional.** Color is reserved for state communication. Visual hierarchy comes from weight, scale, and space. Nothing on the page exists for decoration.

Voice in prose follows the same rules. Short sentences. One thought per sentence. Imperative for instructions. No marketing softeners.

## The monochrome constraint

Aileron v1 has no chromatic brand color. The palette is black, white, and a gray ramp. Status colors exist for green/red/yellow/orange/blue but they are functional. Use them only to signal state.

The constraint is intentional. A monochrome design forces clarity. Hierarchy must come from typography and rhythm. Emphasis must come from weight contrast. A button that demands attention must earn it through its label and size, not through a brand-colored fill.

When a designer or AI agent feels the pull to introduce a brand color, the answer is no. The next iteration of the brand will add color deliberately, after the form is settled. Reach for one of these instead:

- Weight contrast (Aileron Black against Aileron Regular)
- Scale contrast (a 4xl heading next to a sm caption)
- Spatial contrast (a tight cluster surrounded by generous whitespace)
- Elevation contrast (a low-shadowed card on a flat surface)
- Inversion (a foreground/background swap to make a section assert itself)

## Typography

Aileron uses one typeface: **Aileron** by Sora Sagano, served via Adobe Fonts.

The kit ID is `par8udz`. The stylesheet URL is `https://use.typekit.net/par8udz.css`. Both stacks must include this stylesheet in the HTML head. The kit ships weights 100, 200, 300, 400, 700, 800, plus italics. Only Regular (400) and Black (800) are mapped to design roles.

The role assignment is strict:

- **Aileron Regular (400)** carries all prose. Body text, leads, captions, code, UI text.
- **Aileron Black (800)** carries all emphasis. Headings, display type, button labels, badges.

Italic Regular and Italic Black are available for `<em>` and emphasized headings. Other weights load but do not have roles. Resist the pull to introduce Medium or Bold for a "softer" emphasis. Weight contrast does its job because there are only two weights.

### Type scale and roles

The scale in `tokens.json` defines size, line-height, and tracking for each step (`xs` through `7xl`). Components select a **role**, not a raw scale step:

| Role | Scale | Weight | Use for |
|---|---|---|---|
| `display` | 5xl | Black | Hero titles, marketing display lines |
| `h1` | 4xl | Black | Page titles |
| `h2` | 3xl | Black | Major section headings |
| `h3` | 2xl | Black | Subsection headings |
| `h4` | xl | Black | Card titles, table headings |
| `h5` | lg | Black | Smaller groupings |
| `h6` | base | Black | Inline group labels |
| `body` | base | Regular | Default prose |
| `body-lg` | lg | Regular | Lead-out body paragraphs |
| `body-sm` | sm | Regular | Secondary prose, metadata |
| `lead` | xl | Regular | Lead paragraph under a heading |
| `caption` | xs | Regular | Image captions, footnotes |
| `code` | sm | Regular | Inline code, code blocks |
| `ui-label` | sm | Black | Button labels, badge text, table headers |
| `ui-text` | sm | Regular | Tooltips, hint text, secondary UI prose |

Adding a new visual treatment means adding a role to `tokens.json`. Never inline a size or weight in a component.

### Prose rules

Line length is capped per viewport, not per character count. Headings have no max-width.

Body line-height is 1.75 for comfortable long-form reading. Heading line-height varies by scale step (display is `1.1` to clear descenders; smaller headings open up to `1.5`).

Heading tracking is negative (tightening as size increases) per the scale tokens. Body tracking is zero.

Paragraphs have a top margin of `1.25rem` from the preceding sibling. The first paragraph in a section has no top margin.

### Step-snap responsive sizing

Display typography and prose widths use **discrete breakpoints, not fluid scaling**. As the viewport resizes within a breakpoint range, the size and wrap point stay constant. The size jumps at each breakpoint boundary.

This is a deliberate rule. Fluid scaling (`clamp(min, vw, max)`) shifts wrap points continuously, which makes the layout feel unstable during resize. Step-snap behavior produces a predictable size at each breakpoint and a clear visual transition between them.

Applies to:

- `h1` page title — three sizes (small/medium/large) at `< sm`, `sm`–`lg`, `lg+`
- `.prose` max-width — three snap steps at `lg` (1024), `xl` (1280), `2xl` (1536)
- **Brand wordmark in the page header** — one Tailwind step smaller below `lg` than at `lg+` (docs: `text-2xl` → `text-3xl`; webapp: `text-xl` → `text-2xl`)

Below `lg` (mobile, sm, md — anywhere the left sidebar is *not* in the layout) the prose deliberately does *not* snap. Snap widths at those viewports are narrower than the available container, which wastes horizontal space without benefit. The prose uses `max-width: 100%` so it fills the container at whatever the viewport happens to be.

The brand wordmark steps down for the same reason: at `lg+` the page has chrome (sidebar, TOC) framing the content, and a large wordmark anchors the experience. Below `lg` the viewport is content-only — a smaller wordmark leaves more room for the actual page and feels proportional to the simpler layout.

The rule: **scale display elements with available chrome.** When the viewport has layout chrome competing for space (sidebar from `lg`, TOC from `xl`), display elements are bigger and content is snapped to a stable column. When content owns the viewport, display elements step down and content goes fluid.

The `.prose` width ladder accounts for layout changes at each snap breakpoint (left sidebar entering at `lg`, right TOC entering at `xl`).

### Display heading gradient

The page H1 uses a left-to-right linear gradient on the text fill, from full `--foreground` to `--foreground` at 70% opacity, clipped to the glyphs via `background-clip: text`. This gives the title visual weight without introducing color.

Caveats: `background-clip: text` doesn't compose with `filter: drop-shadow`, so the `.ink-bleed` treatment is intentionally skipped on H1. Descender clipping is avoided by setting H1 line-height to `1.1` (not `1.0`), giving glyphs like `g`, `y`, `p` room to render fully.

## Color

The color system is two layers.

The **ramp** lives in `color.brand`. It is the raw palette: brand black, brand white, and an 11-step gray ramp from `gray.50` to `gray.950`. Lightness values are tuned in OKLCH for perceptual evenness across the ramp.

The **semantic** layer lives in `color.mode.light` and `color.mode.dark`. These map ramp values onto shadcn-svelte's conventional token names (background, foreground, muted, primary, secondary, accent, border, card, popover, destructive, sidebar). Components reference semantic names. Components do not reference ramp steps.

Dark mode is not a tinted inversion. It is a separate set of choices in `color.mode.dark` that emphasize different ramp steps for the same semantic role. Test every design in both modes.

### Status color

Five status colors exist: green, red, yellow, orange, blue. Each has a `base` and a `foreground` for accessible text-on-color. Use them only for state:

- **Green**: success, healthy, ready
- **Red**: destructive, error, failed
- **Yellow**: warning, attention required
- **Orange**: degraded, in progress with concern
- **Blue**: informational, neutral notice

Never use a status color as a brand accent or decoration. A green button to "Save" is wrong. The save action is not a success state. The button gets foreground/background like every other primary action.

## Space and rhythm

Spacing is built on a 4px base unit. Tailwind's default scale applies. The values that matter are documented in `spacing.rhythm`:

| Rhythm | Value | Use for |
|---|---|---|
| `section-py-sm` | 4rem | Vertical padding on a section, narrow viewport |
| `section-py-md` | 6rem | Vertical padding on a section, default |
| `section-py-lg` | 8rem | Vertical padding on a section, wide viewport |
| `section-px-sm` | 1.5rem | Horizontal padding on a section, narrow |
| `section-px-md` | 2rem | Horizontal padding on a section, default |
| `section-px-lg` | 3rem | Horizontal padding on a section, wide |
| `content-gap` | 1.5rem | Gap between sub-blocks inside a section |
| `card-padding` | 2rem | Default interior padding on a card |
| `cluster-gap` | 1rem | Gap between buttons or badges in a row |
| `inline-gap` | 0.5rem | Gap between inline elements (icon + label) |

Containers cap at `1536px` (`2xl`) by default. Pages that need wider canvases for tables or grids can add `3xl` (1800px) or `4xl` (2000px) breakpoints, but no v1 layout calls for them.

## Form

### Radius

A single `--radius` of `0.625rem` drives the ramp. `sm` is 4px tighter, `md` is 2px tighter, `lg` is the base, `xl` is 4px looser. `full` is `9999px` for pills.

Components pick a step. Inventing a one-off radius is wrong.

### Borders

Borders are `1px` solid, color `--border`. The border token darkens slightly in dark mode by using a translucent white rather than a fixed gray step, which keeps card edges visible without competing with content.

Avoid borders thicker than 1px. If a separation needs more presence, reach for elevation or a section change, not a heavier line.

### Elevation

Three shadow steps separate planes:

- `--shadow-low`: cards at rest, sidebar nav cards
- `--shadow-mid`: dropdowns, popovers, sticky elements
- `--shadow-high`: modals, sheets, command palettes

A fourth `--shadow-focus` is the focus ring. It uses the `--ring` token, which inverts between modes.

Three steps is the limit. A fourth shadow level would dilute the others.

### Light direction

All shadows cast **45° down-and-to-the-right**, modeling a single light source at the upper-left of the canvas. The shadow's x-offset equals its y-offset (e.g., `--shadow-low` is `10px 10px 15px -3px`).

This rule applies consistently to:

- `box-shadow` on cards, sidebar items, the references panel, the Markdown box
- `filter: drop-shadow` on clipped or filtered shapes
- Inner shadows (when used as inset detail)

A consistent light direction across every surface in the page makes the design feel like one physical environment instead of arbitrary styling.

## Ink-bleed

A subtle halo around text glyphs, simulating ink bleeding into paper. The effect is generated by four 1px directional `drop-shadow` filters (N/S/E/W) at 10% opacity stacked into a single `filter` value.

Opt-in via `class="ink-bleed"`. Applied to a single text element gives that element the bleed; applied to a container, the rule cascades to all text-bearing descendants (`h2`–`h6`, `p`, `li`, `blockquote`).

Currently auto-applied to every section card wrapped by the `rehype-section-wrapper` plugin (the rehype plugin adds both `prose-section` and `ink-bleed` classes to each `<section>`).

Use ink-bleed for: long-form prose sections, body content where you want tactile texture on the type.

Don't use ink-bleed for:

- Gradient-text headings (incompatible with `background-clip: text`)
- UI labels and button text (needs crisp readability)
- Small or dense type (the halo dominates at small sizes)

The values live in `effect.ink-bleed` in `tokens.json` and are exposed as `--effect-ink-bleed` for use directly in CSS. The plain `.ink-bleed` utility class consumes that token.

## Page texture

The page background is not flat. It is a barely-tinted off-white (`color.brand.gray.10`, `oklch(0.995 0 0)`) with a fine linen-weave texture overlaid. The texture is a 4×4px SVG tile of interlocking horizontal and vertical thread segments, filled at 3% opacity and repeated across the page.

At normal viewing distance the texture reads as woven fabric grain. It gives the page a tactile, hosted feel rather than the cold flat surface of an empty white viewport. The weave is light enough that body prose remains comfortable to read at length.

The texture parallaxes against content. A small scroll handler in each stack shifts `background-position-y` at half the scroll rate, so the texture appears to move at 50% the speed of content as the page scrolls. The handler is RAF-throttled and uses a passive listener. JavaScript is required for the parallax; the static texture works without JS.

Content surfaces (`Surface`, `Card`, anything using `--card`) are pure white. The barely-off-white textured page lets those surfaces pop without needing color or heavy elevation. The texture, the tint, and the surface contrast together do the work that a brand color would do in a less constrained palette.

When a section needs to feel hosted by the page rather than floating above it, use `bg-background` directly with no surface wrapper. The texture continues uninterrupted.

Tuning knobs in `pattern.page` if the weave ever needs adjustment:

- **Tile size** (currently 4): smaller = denser weave; larger = more open pattern.
- **Rect dimensions** (currently 2×1 and 1×2 inside the 4×4 tile): controls the thread thickness.
- **`fill-opacity`** (currently `0.03`): visibility. Higher reads as more fabric-like; lower reads closer to plain paper.
- **Parallax rate** (in the scroll handler, currently `0.5`): lower = more parallax. `0` would lock the texture to content; `1` would fix the texture.

## Motion

Motion is a vocabulary of durations and easings. Components pick a pair.

Durations:

- `instant` (0ms): state changes that must not animate (mode switch)
- `fast` (150ms): button presses, focus rings, color changes
- `base` (200ms): hovers, small element transitions
- `slow` (300ms): panel opens, dropdown reveals
- `reveal` (600ms): scroll-into-view, page entrance

Easings:

- `standard`: most transitions
- `enter`: elements appearing
- `exit`: elements leaving
- `spring`: hover lifts and button presses (gives a slight overshoot)

Respect `prefers-reduced-motion`. When set, durations collapse to `instant` and easings are linear. Page-load animations are skipped entirely.

## Layout

The container is centered with horizontal padding that scales with breakpoint. It caps at `2xl` (1536px).

Sections are full-width with horizontal padding equal to the rhythm `section-px-*` values. Section vertical padding is `section-py-*`. A section is the unit of vertical rhythm on a page.

Grids inside sections use the `content-gap` rhythm. Cards inside grids use `card-padding`.

Pages have at most one `display` heading (the page title). Each section starts with an `h2`. Cards inside a section may use `h3` or `h4`.

### Section cards from h2

In the docs site, **every `h2` in MDX content opens a section card**. A rehype plugin (`rehype-section-wrapper`) walks the rendered tree and wraps each `h2` plus its following siblings (until the next `h2`) in a `<section class="prose-section ink-bleed">` element. The CSS for `.prose-section` gives that wrapper a white card surface, `--shadow-low`, and rounded corners. Authors write flat markdown; the section chrome is added at build time.

Content above the first `h2` stays unwrapped at the root of the prose container. Use this space for intro paragraphs and CTAs that should sit directly on the page.

Two MDX-level marker components let authors break out of the auto-wrapping flow:

- **`<SectionBreak />`** — closes the current section. Subsequent content sits outside any card until the next `h2`.
- **`<SectionResume />`** — opens a new section without an `h2`. Useful after a `SectionBreak`, or to open a section before the first `h2`.

Both components render nothing at runtime; the rehype plugin uses them as control markers and removes them from the output. They're globally provided in `[...slug].astro` so authors don't need to import them.

The webapp does not use this pattern — section cards are built directly in component markup rather than generated from h2 boundaries.

## Patterns

The Aileron pattern layer sits above shadcn-svelte primitives. It composes them into reusable section-level shapes (heroes, content sections, brand walls, feature grids).

The full catalog lives in `components.md`. Each entry documents purpose, props, tokens consumed, and stack-specific implementation paths.

## Implementation per stack

Both `docs/` and `webapp/` consume this spec the same way. Implementation differs only in component framework.

### Tokens

Each stack imports `_tokens.generated.css` from `design/build/output/`. The existing `@theme inline` block in `docs/src/styles/global.css` and `webapp/src/app.css` continues to map CSS variables onto Tailwind utilities. Migration of the existing hardcoded variable definitions to the generated file is a follow-up task.

### Typeface

Each stack must include the Adobe Fonts stylesheet in the HTML head, before any rendered content:

```html
<link rel="stylesheet" href="https://use.typekit.net/par8udz.css">
```

In Astro this goes in `BaseLayout.astro`. In SvelteKit this goes in `src/app.html`.

### shadcn-svelte style

Both stacks must use `style: nova` in `components.json`. The webapp currently uses the default style and must be migrated. The migration is one command (`pnpm dlx shadcn-svelte@latest init`) followed by re-installing each component.

### Pattern layer location

Aileron pattern components live at:

- `docs/src/components/aileron/*.astro`
- `webapp/src/lib/components/aileron/*.svelte`

Both follow the contract documented in `components.md` for each pattern.

## Evolving this spec

A token, role, or pattern earns its place in the spec by appearing in two or more places. The first use is a one-off. The second use is a pattern.

Adding a token:

1. Add to `tokens.json` with `$description` explaining the intent
2. Run the generator
3. Update `DESIGN.md` if the new token introduces a category or a use rule
4. Update `components.md` for any component that now consumes it

Adding a pattern:

1. Build it twice in real code first (one use is a special case, two is a pattern)
2. Extract to the pattern layer in both stacks
3. Add an entry to `components.md`
4. Update `DESIGN.md` only if the pattern introduces a new principle

Removing a token or pattern follows the reverse path. Nothing in the spec is permanent. The spec evolves with the product.
