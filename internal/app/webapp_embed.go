package app

import "embed"

// webappFS embeds the local webapp's static-build output. The
// directory is populated by `task build:webapp`, which runs the
// SvelteKit build at `webapp/` (configured with `@sveltejs/adapter-static`)
// and copies the resulting `webapp/build/` into this directory.
//
// `webapp_dist/` is fully gitignored except for a single committed
// `.gitkeep` — that file exists only to satisfy `//go:embed`'s
// compile-time requirement that the pattern match at least one file.
// On a fresh clone where the webapp hasn't been built, `go build`
// still compiles, the daemon still starts, and a request to `/`
// renders an inlined "Aileron webapp not built" stub from
// `webapp_handler.go`'s fallback path. After `task build:webapp`,
// the same handler serves the real shell.
//
// `all:` matters: without it, `go:embed` skips files starting with
// `.` or `_`, which excludes SvelteKit's `_app/` directory entirely
// and our own `.gitkeep`.
//
//go:embed all:webapp_dist
var webappFS embed.FS
