package launch_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestRequestApproval_NoSocket(t *testing.T) {
	decision := launch.RequestApproval("/nonexistent/socket.sock", "echo test", "")
	if decision != "deny" {
		t.Errorf("expected 'deny' when socket unavailable, got %q", decision)
	}
}

func TestApprovalServer_StartAndClose(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "aileron-test-approval.sock")
	t.Cleanup(func() { os.Remove(socketPath) })
	srv, err := launch.NewApprovalServer(socketPath, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	if srv.SocketPath() != socketPath {
		t.Errorf("SocketPath = %q, want %q", srv.SocketPath(), socketPath)
	}
}

func TestApprovalServer_RoundTrip(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "aileron-test-rt.sock")
	t.Cleanup(func() { os.Remove(socketPath) })

	// Create a copier with a mock source that we control.
	srcR, srcW := io.Pipe()
	defer srcW.Close()
	dst := &safeBuf{}
	copier := launch.NewOutputCopier(srcR, dst, nil)
	go copier.Run()

	// Create a key router with a mock stdin.
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	ptmxBuf := &safeBuf{}
	mockOv := &simpleOverlay{}
	router := launch.NewKeyRouter(stdinR, ptmxBuf, mockOv)
	go router.Run()

	srv, err := launch.NewApprovalServer(socketPath, nil, copier, router, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	// Send an approval request in a goroutine.
	done := make(chan string, 1)
	go func() {
		done <- launch.RequestApproval(socketPath, "ls", "")
	}()

	// Wait a bit for the prompt to render, then send 'y' via stdin.
	time.Sleep(200 * time.Millisecond)
	stdinW.Write([]byte("y"))

	select {
	case decision := <-done:
		if decision != "allow_once" {
			t.Errorf("expected 'allow_once', got %q", decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for approval response")
	}
}

func TestApprovalServer_DenyKey(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "aileron-test-deny.sock")
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	srv, _ := launch.NewApprovalServer(socketPath, nil, copier, router, nil)
	go srv.Serve()
	defer srv.Close()

	done := make(chan string, 1)
	go func() {
		done <- launch.RequestApproval(socketPath, "rm -rf /", "dangerous")
	}()

	time.Sleep(200 * time.Millisecond)
	stdinW.Write([]byte("n"))

	select {
	case decision := <-done:
		if decision != "deny" {
			t.Errorf("expected 'deny', got %q", decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestApprovalServer_ProjectKey(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "aileron-test-proj.sock")
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	srv, _ := launch.NewApprovalServer(socketPath, nil, copier, router, nil)
	go srv.Serve()
	defer srv.Close()

	done := make(chan string, 1)
	go func() {
		done <- launch.RequestApproval(socketPath, "git push", "")
	}()

	time.Sleep(200 * time.Millisecond)
	stdinW.Write([]byte("a"))

	select {
	case decision := <-done:
		if decision != "allow_project" {
			t.Errorf("expected 'allow_project', got %q", decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestApprovalServer_UserKey(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "aileron-test-user.sock")
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	srv, _ := launch.NewApprovalServer(socketPath, nil, copier, router, nil)
	go srv.Serve()
	defer srv.Close()

	done := make(chan string, 1)
	go func() {
		done <- launch.RequestApproval(socketPath, "curl api.com", "")
	}()

	time.Sleep(200 * time.Millisecond)
	stdinW.Write([]byte("m"))

	select {
	case decision := <-done:
		if decision != "allow_user" {
			t.Errorf("expected 'allow_user', got %q", decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestApprovalServer_IgnoresUnknownKeys(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "aileron-test-unk.sock")
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	srv, _ := launch.NewApprovalServer(socketPath, nil, copier, router, nil)
	go srv.Serve()
	defer srv.Close()

	done := make(chan string, 1)
	go func() {
		done <- launch.RequestApproval(socketPath, "ls", "")
	}()

	time.Sleep(200 * time.Millisecond)
	// Send garbage keys first, then a valid key.
	stdinW.Write([]byte{0x1B, 'x', 'z'}) // escape + random
	time.Sleep(50 * time.Millisecond)
	stdinW.Write([]byte("y")) // valid

	select {
	case decision := <-done:
		if decision != "allow_once" {
			t.Errorf("expected 'allow_once' after ignoring unknown keys, got %q", decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestApprovalServer_PromptContainsCommand(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "aileron-test-cmd.sock")
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	dst := &safeBuf{}
	copier := launch.NewOutputCopier(srcR, dst, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	srv, _ := launch.NewApprovalServer(socketPath, nil, copier, router, nil)
	go srv.Serve()
	defer srv.Close()

	done := make(chan string, 1)
	go func() {
		done <- launch.RequestApproval(socketPath, "git push origin main", "")
	}()

	time.Sleep(200 * time.Millisecond)
	stdinW.Write([]byte("n"))
	<-done

	// The prompt should have been written to dst via WriteExclusive.
	out := dst.String()
	if !strings.Contains(out, "git push origin main") {
		t.Errorf("expected command in prompt output, got %q", out)
	}
	if !strings.Contains(out, "Aileron") {
		t.Errorf("expected 'Aileron' branding in prompt, got %q", out)
	}
}

func TestOutputCopier_WriteExclusive(t *testing.T) {
	srcR, srcW := io.Pipe()
	dst := &safeBuf{}
	oc := launch.NewOutputCopier(srcR, dst, nil)
	go oc.Run()

	// Send a direct write.
	oc.WriteExclusive([]byte("exclusive content"))
	time.Sleep(50 * time.Millisecond)

	if !strings.Contains(dst.String(), "exclusive content") {
		t.Errorf("expected 'exclusive content' in output, got %q", dst.String())
	}

	srcW.Close()
}

func TestOutputCopier_SetPaused(t *testing.T) {
	srcR, srcW := io.Pipe()
	dst := &safeBuf{}
	oc := launch.NewOutputCopier(srcR, dst, nil)
	go oc.Run()

	// Write some normal data.
	srcW.Write([]byte("before"))
	time.Sleep(50 * time.Millisecond)

	// Pause and write more data — should be buffered.
	oc.SetPaused(true)
	srcW.Write([]byte("during"))
	time.Sleep(100 * time.Millisecond)

	if strings.Contains(dst.String(), "during") {
		t.Error("'during' should be buffered while paused")
	}

	// Unpause — buffered data should flush.
	oc.SetPaused(false)
	time.Sleep(50 * time.Millisecond)

	if !strings.Contains(dst.String(), "during") {
		t.Error("expected 'during' after unpause")
	}

	srcW.Close()
}

func TestKeyRouter_StealAndRelease(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &simpleOverlay{}
	router := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go router.Run()

	// Steal input.
	ch := router.StealInput()

	// Keys should go to the stolen channel, not the pty.
	stdinW.Write([]byte("x"))
	select {
	case b := <-ch:
		if b != 'x' {
			t.Errorf("expected 'x', got %c", b)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stolen input")
	}

	if ptmxBuf.Len() > 0 {
		t.Error("keys should not reach pty while input is stolen")
	}

	// Release — keys should go back to pty.
	router.ReleaseInput()
	stdinW.Write([]byte("z"))
	time.Sleep(50 * time.Millisecond)

	if !strings.Contains(ptmxBuf.String(), "z") {
		t.Errorf("expected 'z' in pty after release, got %q", ptmxBuf.String())
	}

	stdinW.Close()
}

func TestStatusBar_Dims(t *testing.T) {
	bar := launch.NewStatusBar(30, 120, "test")
	rows, cols := bar.Dims()
	if rows != 30 || cols != 120 {
		t.Errorf("Dims = (%d, %d), want (30, 120)", rows, cols)
	}

	// After resize, dims should update.
	var buf strings.Builder
	bar.Resize(&buf, 40, 160)
	rows, cols = bar.Dims()
	if rows != 40 || cols != 160 {
		t.Errorf("Dims after resize = (%d, %d), want (40, 160)", rows, cols)
	}
}
