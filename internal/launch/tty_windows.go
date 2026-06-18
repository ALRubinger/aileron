//go:build windows

package launch

import "os"

// openControllingTerminal opens the controlling terminal for interactive
// passphrase prompts. Windows has no /dev/tty; the documented equivalent is
// the console device pair CONIN$ (input) and CONOUT$ (output), which refer
// to the process's attached console regardless of stdin/stdout redirection.
//
// We open CONIN$ read/write so the returned handle can both read keystrokes
// and have echo toggled. The returned *os.File's Fd() is a valid console
// handle for golang.org/x/term.ReadPassword, which uses the Windows console
// mode APIs (GetConsoleMode/SetConsoleMode) under the hood. The caller owns
// the file and must Close it.
func openControllingTerminal() (*os.File, error) {
	return os.OpenFile("CONIN$", os.O_RDWR, 0)
}
