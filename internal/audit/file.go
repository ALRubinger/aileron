package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/ALRubinger/aileron/internal/model"
)

// FileStore is a JSONL-backed audit store: one event per line,
// append-only, durable across daemon restarts. Reads are served
// from an in-memory index rebuilt at startup, so the on-disk file is
// authoritative and the in-memory copy is a cache.
//
// Per ADR-0010 this is the v0.x persistence target — Postgres /
// signed log are post-MVP and operate over the same SPI. Rotation,
// retention, and dedup are deliberately not implemented.
type FileStore struct {
	path string
	log  *slog.Logger
	mu   sync.Mutex // serializes appends so file order matches mem order
	mem  *MemStore  // hydrated from the file at New, then write-through
}

// fileLine is the on-disk JSONL shape: the SPI Event plus the
// optional trace-correlation fields. Mirroring [eventWithTrace] keeps
// trace-aware Append lossless across restart.
type fileLine struct {
	TraceID     string `json:"trace_id,omitempty"`
	IntentID    string `json:"intent_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Event       Event  `json:"event"`
}

// NewFileStore opens (or creates) a JSONL audit log at path and
// returns a FileStore whose in-memory index is replayed from the
// file. Malformed lines are skipped with a warning so a single
// corrupt entry can't make the daemon refuse to start; the rest of
// the file is still loaded.
//
// log may be nil; in that case warnings are routed to slog.Default.
func NewFileStore(path string, log *slog.Logger) (*FileStore, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating audit log directory: %w", err)
	}
	fs := &FileStore{
		path: path,
		log:  log,
		mem:  NewMemStore(),
	}
	if err := fs.replay(); err != nil {
		return nil, err
	}
	return fs, nil
}

// Path returns the JSONL file path the store is backed by. Useful
// for status surfaces (e.g. `aileron status`) and tests.
func (fs *FileStore) Path() string { return fs.path }

func (fs *FileStore) replay() error {
	f, err := os.Open(fs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("opening audit log: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// Default Scanner buffer is 64KiB; raise to handle large
	// payloads (failure events with deep details). 1 MiB cap.
	const maxLine = 1 << 20
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)
	lineNo := 0
	bad := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var line fileLine
		if err := json.Unmarshal(raw, &line); err != nil {
			bad++
			fs.log.Warn("audit: skipped malformed line during replay",
				"path", fs.path, "line", lineNo, "error", err.Error())
			continue
		}
		if line.TraceID == "" && line.IntentID == "" && line.WorkspaceID == "" {
			_ = fs.mem.Append(context.Background(), line.Event)
		} else {
			_ = fs.mem.AppendToTrace(context.Background(),
				line.TraceID, line.IntentID, line.WorkspaceID, line.Event)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning audit log: %w", err)
	}
	if bad > 0 {
		fs.log.Warn("audit: replay completed with skipped lines",
			"path", fs.path, "skipped", bad, "loaded", lineNo-bad)
	}
	return nil
}

// Append persists the event to the file and the in-memory index.
// Both must succeed for Append to return nil; a write to disk that
// fails surfaces to the recorder, which logs and continues per
// ADR-0010's best-effort recording.
func (fs *FileStore) Append(ctx context.Context, event Event) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.writeLine(fileLine{Event: event}); err != nil {
		return err
	}
	return fs.mem.Append(ctx, event)
}

// AppendToTrace persists the trace-correlated event to the file and
// the in-memory index.
func (fs *FileStore) AppendToTrace(ctx context.Context, traceID, intentID, workspaceID string, event Event) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.writeLine(fileLine{
		TraceID:     traceID,
		IntentID:    intentID,
		WorkspaceID: workspaceID,
		Event:       event,
	}); err != nil {
		return err
	}
	return fs.mem.AppendToTrace(ctx, traceID, intentID, workspaceID, event)
}

func (fs *FileStore) writeLine(line fileLine) error {
	f, err := os.OpenFile(fs.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening audit log: %w", err)
	}
	defer f.Close()
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("encoding audit entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing audit entry: %w", err)
	}
	return nil
}

// GetByEventID delegates to the in-memory index.
func (fs *FileStore) GetByEventID(eventID string) (Event, bool) {
	return fs.mem.GetByEventID(eventID)
}

// ListEvents delegates to the in-memory index.
func (fs *FileStore) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	return fs.mem.ListEvents(ctx, f)
}

// ListTraces delegates to the in-memory index.
func (fs *FileStore) ListTraces(ctx context.Context, filter Filter) ([]Trace, error) {
	return fs.mem.ListTraces(ctx, filter)
}

// GetTrace delegates to the in-memory index.
func (fs *FileStore) GetTrace(ctx context.Context, traceID string) (Trace, error) {
	return fs.mem.GetTrace(ctx, traceID)
}

// Compile-time check that FileStore satisfies the audit-store
// surface. MemStore satisfies this implicitly today.
var (
	_ EventStore = (*FileStore)(nil)
	_ EventStore = (*MemStore)(nil)
	_ Store      = (*FileStore)(nil)

	// helperEnsureModelImported keeps the model import live regardless
	// of whether other callers in this package use it directly. Same
	// purpose as the equivalent line in mem.go.
	_ = model.ActorRef{}
)
