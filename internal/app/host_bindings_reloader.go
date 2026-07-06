package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// currentHostBindings returns the host-binding table the forward proxy should
// match against. When a reloader is wired (production boot), it returns the
// reloader's live table, re-reading the user descriptor file first if it
// changed on disk (#1887). When no reloader is set (tests that populate
// hostBindings directly, and any pre-reloader construction path), it returns
// the static table, preserving today's behavior.
func (s *apiServer) currentHostBindings() binding.HostBindings {
	if s.hostBindingsReloader != nil {
		return s.hostBindingsReloader.current()
	}
	return s.hostBindings
}

// hostBindingsReloader holds the daemon's host->credential binding table and
// re-reads the user descriptor file (~/.aileron/binding-descriptors.yaml) when
// it changes on disk, so out-of-band edits — including `aileron skill bind`
// writes — take effect at the forward-proxy boundary without a daemon restart
// (#1887). Before this, the table was assembled once at boot and never
// reassigned, so a `skill bind` write and the running daemon silently diverged
// until a restart.
//
// Reload strategy is re-stat, not fsnotify: the proxy access path os.Stats the
// user file (mtime+size) and only re-parses when the signature changes. A
// steady-state CONNECT costs one stat syscall and no parse; a parse happens
// only after an actual edit. This is the simpler path the issue endorses over a
// filesystem watcher.
//
// The image-derived unit layer (#1322) is resolved once at boot and cached in
// extraEntries: a reload re-reads only the user file and never re-resolves the
// image via daemonUnitLayers, so a reload can never block on a container
// runtime and the sealing layer stays stable for the daemon's lifetime.
//
// A reload that fails (malformed/placeholder descriptor — the same load-time
// validation as #1873) keeps the previously-loaded table and surfaces the error
// via the log rather than half-applying or degrading to an empty (passthrough)
// table. The stat signature is NOT advanced on failure, so the next access
// retries; once the operator fixes the file the reload succeeds.
//
// This mirrors the reload precedent already in the daemon: ReloadingKeyring
// (verify.go) re-reads the keyring so `keyring trust` writes take effect, and
// the action store reloads after an install (handlers_install_actions.go).
type hostBindingsReloader struct {
	userPath     string
	extraEntries []proxybinding.Entry
	log          *slog.Logger

	mu    sync.Mutex
	table binding.HostBindings
	// sig is the stat signature of the user file as of the last successful
	// (or initial) load. A zero-value sig means the file was absent then.
	sig hostBindingsStatSig
}

// hostBindingsStatSig is the change-detection signature for the user descriptor
// file: presence plus mtime and size. An edit that changes content changes at
// least one of mtime or size, so this distinguishes an unchanged file (skip the
// re-parse) from an edited one (reload). Absence is a first-class state so
// creating or deleting the file is detected too.
type hostBindingsStatSig struct {
	exists  bool
	modTime time.Time
	size    int64
}

func (a hostBindingsStatSig) equal(b hostBindingsStatSig) bool {
	return a.exists == b.exists && a.size == b.size && a.modTime.Equal(b.modTime)
}

// newHostBindingsReloader resolves the image-derived unit layer once, performs
// the initial load, and returns a reloader primed to observe later edits to the
// user descriptor file. An initial load error (malformed image unit or a
// malformed descriptor in any layer) is surfaced so the daemon fails boot
// loudly, exactly as the previous one-shot assembly did — a broken descriptor
// must never silently ship an empty (passthrough) table.
func newHostBindingsReloader(ctx context.Context, log *slog.Logger) (*hostBindingsReloader, error) {
	_, sealingLayer, err := daemonUnitLayers(ctx)
	if err != nil {
		return nil, fmt.Errorf("assemble host bindings: load image unit layer: %w", err)
	}
	r := &hostBindingsReloader{
		userPath:     proxybinding.DefaultUserPath(),
		extraEntries: sealingLayer,
		log:          log,
	}
	// Record the stat before the initial load so an edit that races the load is
	// not silently missed: if the file changes after this stat but before or
	// during the load, the recorded (older) signature differs from the next
	// access's stat and triggers a redundant-but-correct reload.
	sig := r.statUserFile()
	table, err := r.loadTable()
	if err != nil {
		return nil, fmt.Errorf("assemble host bindings: %w", err)
	}
	r.table = table
	r.sig = sig
	return r, nil
}

// statUserFile returns the current change-detection signature for the user
// descriptor file. An absent or unstattable file yields the zero signature: an
// absent user layer is valid (built-in defaults only), matching the loader's
// missing-file semantics, so absence is not an error here.
func (r *hostBindingsReloader) statUserFile() hostBindingsStatSig {
	if r.userPath == "" {
		return hostBindingsStatSig{}
	}
	fi, err := os.Stat(r.userPath)
	if err != nil {
		return hostBindingsStatSig{}
	}
	return hostBindingsStatSig{exists: true, modTime: fi.ModTime(), size: fi.Size()}
}

// loadTable re-reads the user descriptor layer, merges it with the cached image
// unit layer and the embedded built-in defaults, and adapts the result to the
// canonical binding table. Non-fatal warnings (a well-formed-but-suspect
// binding) are logged, matching the boot-time warn-only contract; a malformed
// layer returns an error and no table.
func (r *hostBindingsReloader) loadTable() (binding.HostBindings, error) {
	opts := proxybinding.LoadOptions{
		UserPath:     r.userPath,
		ExtraEntries: r.extraEntries,
	}
	table, warnings, err := proxybinding.LoadHostBindingsWithWarnings(opts)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		if r.log != nil {
			r.log.Warn("host binding descriptor warning", "detail", w)
		}
	}
	return table, nil
}

// current returns the live host-binding table, reloading it first if the user
// descriptor file changed since the last load. It is called on the concurrent
// proxy access path, so it serializes stat+reload+swap under a mutex; the stat
// itself is done before the lock to keep the common (unchanged) case cheap.
//
// On a reload failure the previous table is returned unchanged and the error is
// logged: a bad edit degrades to "stale but serving", never to an empty table,
// and never half-applies a partially-valid document.
func (r *hostBindingsReloader) current() binding.HostBindings {
	sig := r.statUserFile()

	r.mu.Lock()
	defer r.mu.Unlock()

	if sig.equal(r.sig) {
		return r.table
	}

	table, err := r.loadTable()
	if err != nil {
		if r.log != nil {
			r.log.Error("host binding descriptor reload failed; keeping previous table",
				"path", r.userPath, "error", err)
		}
		// Do not advance r.sig: the next access retries the reload, so a fixed
		// file is picked up without waiting for a further edit.
		return r.table
	}
	r.table = table
	r.sig = sig
	return table
}
