package launch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestRequestApproval_NoSocket(t *testing.T) {
	// No socket → should return "deny" without error.
	decision := launch.RequestApproval("/nonexistent/socket.sock", "echo test", "")
	if decision != "deny" {
		t.Errorf("expected 'deny' when socket unavailable, got %q", decision)
	}
}

func TestApprovalServer_StartAndClose(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "aileron-test-approval.sock")
	t.Cleanup(func() { os.Remove(socketPath) })
	srv, err := launch.NewApprovalServer(socketPath, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	// Server is running — verify socket path.
	if srv.SocketPath() != socketPath {
		t.Errorf("SocketPath = %q, want %q", srv.SocketPath(), socketPath)
	}
}
