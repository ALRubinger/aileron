package app

import "embed"

// webappFS embeds the local webapp's static-build output. The
// directory is populated by `task build:webapp`, which runs the
// SvelteKit build at `webapp/` (configured with `@sveltejs/adapter-static`)
// and copies the resulting `webapp/build/` into this directory.
//
// A stub `index.html` is committed alongside this file so plain
// `go build` always succeeds, even on a fresh clone where the
// webapp hasn't been built yet — the daemon then serves a "Aileron
// webapp not built" page that points the operator at the build
// command. CI / `task build` runs the webapp build before `go build`,
// so production binaries always carry the real assets. The
// .gitignore pattern under `internal/app/webapp_dist/` excludes
// every file except the committed stub `index.html`.
//
// `all:` matters: without it, `go:embed` skips files starting with
// `.` or `_`, which excludes SvelteKit's `_app/` directory entirely.
//
//go:embed all:webapp_dist
var webappFS embed.FS
