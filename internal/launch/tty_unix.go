//go:build unix

package launch

import "os"

// openControllingTerminal opens the controlling terminal for interactive
// passphrase prompts. On POSIX systems this is /dev/tty, which refers to
// the process's controlling terminal regardless of stdin/stdout redirection.
//
// The returned *os.File's Fd() is a valid terminal file descriptor for
// golang.org/x/term.ReadPassword. The caller owns the file and must Close it.
func openControllingTerminal() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
