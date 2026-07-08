package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// fakeWalker is a test InputWalker that merges a canned set of collected values
// into the launch args and records the inputs it was handed, so a test asserts
// both that the walk ran and that its output reached the downstream boot/resolve
// path. A nil collected map records the call without changing the args.
type fakeWalker struct {
	collected map[string]any
	gotInputs []Input
	called    bool
}

func (f *fakeWalker) Walk(inputs []Input, args LaunchArgs) (LaunchArgs, error) {
	f.called = true
	f.gotInputs = inputs
	out := LaunchArgs{}
	for k, v := range args {
		out[k] = v
	}
	for k, v := range f.collected {
		out[k] = v
	}
	return out, nil
}

// A wired InputWalker runs host-side on the SEALED-IMAGE path and its collected
// values reach the ImageRunner's spec.Inputs. This is the acceptance-critical
// proof the walk is NOT inert on the boot mainline: the in-container prompter is
// never consulted for a whole-plan-pinned unit, so only the host-side walk can
// feed inputs into the boot.
func TestRun_InputWalkerFeedsSealedImageBoot(t *testing.T) {
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	fake := &fakeImageRunner{result: ImageRunResult{ContentHash: "sha256:booted"}}
	// window_days is the plan's one literal input; the walk collects a typed
	// default-native value for it (14, a number), which must reach the boot.
	walker := &fakeWalker{collected: map[string]any{"window_days": 14}}
	_, err := Run(context.Background(), Options{
		Store:       s,
		Name:        "weekly-metrics-digest",
		Version:     "test",
		ImageRunner: fake,
		InputWalker: walker,
	})
	if err != nil {
		t.Fatalf("Run boot path: %v", err)
	}
	if !walker.called {
		t.Fatal("the InputWalker must run host-side before the image boot")
	}
	if !fake.called {
		t.Fatal("Run did not delegate to the ImageRunner")
	}
	if got := fake.spec.Inputs["window_days"]; got != 14 {
		t.Errorf("walked value did not reach the boot spec: spec.Inputs = %v", fake.spec.Inputs)
	}
	// The walk was handed the plan's declared inputs so it can render every one.
	if len(walker.gotInputs) == 0 {
		t.Error("the walker must be handed the plan's declared inputs")
	}
}

// The in-container image-boot re-entry (InPinnedImage) must NOT invoke the
// walker: the structural no-recursion guard is the InPinnedImage gate, not the
// container's non-TTY state.
func TestRun_InPinnedImageReentrySkipsWalker(t *testing.T) {
	fv := frozenExample(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	walker := &fakeWalker{collected: map[string]any{"window_days": 99}}
	_, err := Run(context.Background(), Options{
		Store:         s,
		Name:          "weekly-metrics-digest",
		Version:       "test",
		InPinnedImage: true,
		InputWalker:   walker,
		Dispatcher:    disp,
		Approver:      &fakeApprover{decision: Decision{Approved: true}},
		Seam:          fakeSeam{out: map[string]any{"issue_body": "x"}},
		Clock:         FixedClock{},
		Transforms:    reg,
		OutDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run in-container re-entry: %v", err)
	}
	if walker.called {
		t.Fatal("the image-boot re-entry (InPinnedImage) must never invoke the walker")
	}
}

// A tool-step plan with no pinned environment refuses BEFORE the interactive
// input walk runs (#2063). The fail-closed no-environment refusal (#1829) is
// unconditional for such a plan, so dragging the operator through a guided walk
// whose result can only be discarded is wasted work: the refusal must precede
// the walk. The walker must never be called.
func TestRun_ToolStepNoImageRefusesBeforeWalk(t *testing.T) {
	fv := frozenToolStep(t, false)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("tool-step-plan", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	walker := &fakeWalker{collected: map[string]any{"window_days": 7}}
	_, err := Run(context.Background(), Options{
		Store:       s,
		Name:        "tool-step-plan",
		Version:     "test",
		ToolRunner:  &fakeToolStepRunner{},
		InputWalker: walker,
	})
	if err == nil {
		t.Fatal("a tool-step plan with no pinned environment must refuse")
	}
	if !strings.Contains(err.Error(), "pins no environment image") {
		t.Errorf("error = %v, want the no-environment refusal", err)
	}
	if walker.called {
		t.Fatal("the no-environment refusal must fire BEFORE the interactive walk; the walker must never run")
	}
}

// On the in-process path (no pinned image) the walked values flow through
// resolveInputs as overrides and land in the resolved-input set.
func TestRun_InputWalkerFeedsInProcessResolve(t *testing.T) {
	fv := frozenNoImage(t)
	s := store.New(t.TempDir())
	if err := s.WriteFrozen("no-exec-env", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	reg := NewTransformRegistry()
	reg.Register("identity", func(b map[string]any, outs []string) (map[string]any, error) {
		return map[string]any{outs[0]: map[string]any{"encoding": "utf-8", "content": "name\ncpu\n", "mimeType": "text/csv"}}, nil
	})
	disp := &dispatchRouter{results: map[string]map[string]any{
		"aileron:metrics.query_series": {"series": []any{map[string]any{"name": "cpu"}}},
		"aileron:tracker.create_issue": {"encoding": "utf-8", "content": "{}", "mimeType": "application/json"},
	}}
	walker := &fakeWalker{collected: map[string]any{"window_days": "30"}}
	res, err := Run(context.Background(), Options{
		Store:       s,
		Name:        "no-exec-env",
		Version:     "test",
		InputWalker: walker,
		Dispatcher:  disp,
		Approver:    &fakeApprover{decision: Decision{Approved: true}},
		Seam:        fakeSeam{out: map[string]any{"issue_body": "x"}},
		Clock:       FixedClock{},
		Transforms:  reg,
		OutDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run in-process path: %v", err)
	}
	if !walker.called {
		t.Fatal("the InputWalker must run on the in-process path")
	}
	if got := res.ResolvedInputs["window_days"]; got != "30" {
		t.Errorf("walked override did not reach the resolved inputs: got %v", got)
	}
}
