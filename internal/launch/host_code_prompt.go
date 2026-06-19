package launch

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// defaultHostCodePrompter is the launcher's production CodePrompter for
// the host-side hosted-callback ("paste the code") flow. It writes the
// human-facing prompt to promptW (the launcher's stderr) and reads one
// line from the process's CONTROLLING TERMINAL — not stdin.
//
// Reading from the controlling terminal (rather than stdin) is what
// keeps the paste on the HOST, even when the launcher's stdin is wired
// to the container's PTY. This is the original Windows-blocker fix: the
// user pastes the code into their own terminal, never into the
// container TTY.
//
// The pasted authorization code is NOT a secret in the password sense
// (it is single-use and short-lived) and can be long, so we read a
// plain line with bufio.Reader.ReadString('\n') rather than
// term.ReadPassword (which suppresses echo and caps line length on some
// platforms). The returned value is whitespace-trimmed.
//
// ctx cancellation is honored on a best-effort basis: a blocked
// terminal read cannot itself be interrupted, but if the context is
// already done when the prompter is entered we return immediately
// without prompting so a cancelled launch does not paint a stray
// prompt.
func defaultHostCodePrompter(ctx context.Context, promptW io.Writer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tty, err := openControllingTerminal()
	if err != nil {
		return "", fmt.Errorf("open controlling terminal for code paste: %w", err)
	}
	defer tty.Close()

	if promptW != nil {
		fmt.Fprint(promptW, "Paste the code from your browser and press Enter: ")
	}

	line, err := bufio.NewReader(tty).ReadString('\n')
	// ReadString returns the data read so far together with io.EOF when
	// the terminal is closed before a newline; a pasted-then-EOF code is
	// still usable, so only treat a non-EOF error as fatal.
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read pasted code from terminal: %w", err)
	}
	code := strings.TrimSpace(line)
	if code == "" {
		return "", fmt.Errorf("no code entered")
	}
	return code, nil
}
