package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
)

// --- parseOpenArgs contract ---

func TestParseOpenArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantTarget  openTarget
		wantID      string
		wantErrFrag string
	}{
		{"no args → root", nil, openTargetRoot, "", ""},
		{"empty slice → root", []string{}, openTargetRoot, "", ""},
		{"approvals plain", []string{"approvals"}, openTargetApprovals, "", ""},
		{"approval with id", []string{"approval", "act-42"}, openTargetApproval, "act-42", ""},
		{"approval with id containing slash", []string{"approval", "scope/act-1"}, openTargetApproval, "scope/act-1", ""},
		{"approval without id", []string{"approval"}, 0, "", "requires an approval ID"},
		{"approval with empty id", []string{"approval", ""}, 0, "", "requires an approval ID"},
		{"approval with extra arg", []string{"approval", "act-1", "extra"}, 0, "", "takes one argument"},
		{"approvals with stray id", []string{"approvals", "act-1"}, 0, "", "did you mean `open approval"},
		{"unknown target", []string{"webapp"}, 0, "", "unknown open target"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt, id, err := parseOpenArgs(c.args)
			if c.wantErrFrag != "" {
				if err == nil {
					t.Fatalf("expected error containing %q; got nil", c.wantErrFrag)
				}
				if !strings.Contains(err.Error(), c.wantErrFrag) {
					t.Errorf("error = %q, want substring %q", err.Error(), c.wantErrFrag)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tgt != c.wantTarget {
				t.Errorf("target = %v, want %v", tgt, c.wantTarget)
			}
			if id != c.wantID {
				t.Errorf("id = %q, want %q", id, c.wantID)
			}
		})
	}
}

// --- buildOpenURL contract ---

func TestBuildOpenURL(t *testing.T) {
	cases := []struct {
		name       string
		base       string
		target     openTarget
		approvalID string
		want       string
	}{
		{"root", "http://127.0.0.1:50419", openTargetRoot, "", "http://127.0.0.1:50419/"},
		{"root strips trailing slash", "http://127.0.0.1:50419/", openTargetRoot, "", "http://127.0.0.1:50419/"},
		{"approvals", "http://127.0.0.1:50419", openTargetApprovals, "", "http://127.0.0.1:50419/approvals"},
		{"approvals strips trailing slash", "http://127.0.0.1:50419/", openTargetApprovals, "", "http://127.0.0.1:50419/approvals"},
		{"approval with id", "http://127.0.0.1:50419", openTargetApproval, "act-42", "http://127.0.0.1:50419/approvals?focus=act-42"},
		{"approval id is query-escaped", "http://127.0.0.1:50419", openTargetApproval, "scope/act 1", "http://127.0.0.1:50419/approvals?focus=scope%2Fact+1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildOpenURL(c.base, c.target, c.approvalID); got != c.want {
				t.Errorf("buildOpenURL(%q, %v, %q) = %q, want %q",
					c.base, c.target, c.approvalID, got, c.want)
			}
		})
	}
}

// --- escapeURLForWindowsCmd contract ---

// TestEscapeURLForWindowsCmd is the regression test for the `aileron
// open` opener carrying the same cmd.exe-truncation defect as the OAuth
// browser opener: a multi-parameter URL handed to `cmd /c start ""
// <url>` is cut off at the first `&` because cmd treats it as a command
// separator. The metacharacters must be caret-escaped so the whole URL
// survives. Today `aileron open`'s URLs carry at most one query param so
// the bug is latent, but the escaping must still hold every parameter.
func TestEscapeURLForWindowsCmd(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain URL unchanged", "http://127.0.0.1:50419/approvals", "http://127.0.0.1:50419/approvals"},
		{"single query param unchanged", "http://127.0.0.1:50419/approvals?focus=act-42", "http://127.0.0.1:50419/approvals?focus=act-42"},
		{"multi param ampersands escaped", "http://h/approvals?focus=a&tab=b&sort=c", "http://h/approvals?focus=a^&tab=b^&sort=c"},
		{"all metacharacters", `&|<>()%`, `^&^|^<^>^(^)^%`},
		{"caret escaped first", "a^&b", "a^^^&b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := escapeURLForWindowsCmd(c.in)
			if got != c.want {
				t.Errorf("escapeURLForWindowsCmd(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}

	// Spot-check the load-bearing property: every parameter of a
	// multi-param URL survives the escape (the bug dropped all but the
	// first).
	const multi = "http://h/approvals?focus=act-42&tab=open&sort=newest"
	escaped := escapeURLForWindowsCmd(multi)
	for _, frag := range []string{"focus=act-42", "tab=open", "sort=newest"} {
		if !strings.Contains(escaped, frag) {
			t.Errorf("escaped %q missing %q", escaped, frag)
		}
	}
}

// --- runOpen ---

// stubOpener captures the URL passed to the browser opener and
// optionally returns an error to simulate an unavailable opener.
type stubOpener struct {
	called []string
	err    error
}

func (s *stubOpener) open(_ context.Context, target string) error {
	s.called = append(s.called, target)
	return s.err
}

// withStubs replaces the package-level test seams for the duration of
// a test and restores them on cleanup. The state-dir resolver returns
// "" so a misuse — discoveryReadFn that depends on it — would surface
// loudly rather than silently passing.
func withStubs(t *testing.T, info discovery.Info, readErr error, openErr error) *stubOpener {
	t.Helper()
	prevState := defaultStateDirFn
	prevRead := discoveryReadFn
	prevOpener := openInBrowserFn

	defaultStateDirFn = func() (string, error) { return "/test/stateDir", nil }
	discoveryReadFn = func(_ string) (discovery.Info, error) {
		if readErr != nil {
			return discovery.Info{}, readErr
		}
		return info, nil
	}
	opener := &stubOpener{err: openErr}
	openInBrowserFn = opener.open

	t.Cleanup(func() {
		defaultStateDirFn = prevState
		discoveryReadFn = prevRead
		openInBrowserFn = prevOpener
	})
	return opener
}

func TestRunOpen_RootOpensWebappBase(t *testing.T) {
	info := discovery.Info{URL: "http://127.0.0.1:50419", PID: 1, Version: "test", StartedAt: time.Now()}
	opener := withStubs(t, info, nil, nil)

	var stdout, stderr bytes.Buffer
	code := runOpen(nil, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(opener.called) != 1 {
		t.Fatalf("opener.called = %v, want 1 invocation", opener.called)
	}
	if opener.called[0] != "http://127.0.0.1:50419/" {
		t.Errorf("opened %q, want http://127.0.0.1:50419/", opener.called[0])
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:50419/") {
		t.Errorf("stdout missing URL; got %q", stdout.String())
	}
}

func TestRunOpen_ApprovalsOpensListPage(t *testing.T) {
	info := discovery.Info{URL: "http://127.0.0.1:50419"}
	opener := withStubs(t, info, nil, nil)

	var stdout, stderr bytes.Buffer
	code := runOpen([]string{"approvals"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if got := opener.called[0]; got != "http://127.0.0.1:50419/approvals" {
		t.Errorf("opened %q, want http://127.0.0.1:50419/approvals", got)
	}
}

func TestRunOpen_ApprovalOpensFocusPage(t *testing.T) {
	info := discovery.Info{URL: "http://127.0.0.1:50419"}
	opener := withStubs(t, info, nil, nil)

	var stdout, stderr bytes.Buffer
	code := runOpen([]string{"approval", "act-42"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if got := opener.called[0]; got != "http://127.0.0.1:50419/approvals?focus=act-42" {
		t.Errorf("opened %q, want http://127.0.0.1:50419/approvals?focus=act-42", got)
	}
}

func TestRunOpen_DaemonNotRunningReturns1AndPrintsHint(t *testing.T) {
	withStubs(t, discovery.Info{}, discovery.ErrNotRunning, nil)

	var stdout, stderr bytes.Buffer
	code := runOpen(nil, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "daemon is not running") {
		t.Errorf("stderr missing 'daemon is not running' hint; got %q", stderr.String())
	}
}

func TestRunOpen_OpenerFailurePrintsURLForCopyPaste(t *testing.T) {
	info := discovery.Info{URL: "http://127.0.0.1:50419"}
	withStubs(t, info, nil, errors.New("xdg-open: not found"))

	var stdout, stderr bytes.Buffer
	code := runOpen([]string{"approval", "act-1"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	// When the OS opener fails the user can't see anything land in a
	// browser; falling back to printing the URL so they can copy-paste
	// is the load-bearing recovery path. Without it the user is left
	// with only the failure message and no way forward.
	if !strings.Contains(stderr.String(), "http://127.0.0.1:50419/approvals?focus=act-1") {
		t.Errorf("stderr missing copy-paste URL; got %q", stderr.String())
	}
}

func TestRunOpen_BadArgsReturns1AndPrintsUsage(t *testing.T) {
	withStubs(t, discovery.Info{URL: "http://127.0.0.1:50419"}, nil, nil)

	var stdout, stderr bytes.Buffer
	code := runOpen([]string{"unknown-target"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr missing usage line; got %q", stderr.String())
	}
}

// TestRunOpen_StateDirErrorReturns1 covers the rare case where the
// home directory can't be resolved (e.g. unset HOME on Unix). The
// command exits 1 and surfaces the underlying error rather than
// continuing on with an empty stateDir.
func TestRunOpen_StateDirErrorReturns1(t *testing.T) {
	prev := defaultStateDirFn
	defaultStateDirFn = func() (string, error) { return "", errors.New("no home directory") }
	t.Cleanup(func() { defaultStateDirFn = prev })

	var stdout, stderr bytes.Buffer
	code := runOpen(nil, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no home directory") {
		t.Errorf("stderr should surface state-dir error; got %q", stderr.String())
	}
}

// TestRunOpen_DiscoveryReadGenericErrorReturns1 covers the non-
// ErrNotRunning failure path (e.g. permission denied on daemon.json):
// the error is wrapped and exits 1 without the "not running" hint
// (which would mislead the operator).
func TestRunOpen_DiscoveryReadGenericErrorReturns1(t *testing.T) {
	withStubs(t, discovery.Info{}, errors.New("permission denied"), nil)

	var stdout, stderr bytes.Buffer
	code := runOpen(nil, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if strings.Contains(stderr.String(), "is not running") {
		t.Errorf("stderr should not mislead with 'not running' on generic error; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr should include underlying error; got %q", stderr.String())
	}
}
