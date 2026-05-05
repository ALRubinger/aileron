package launch

// OpenSessionLogger exposes openSessionLogger for testing.
var OpenSessionLogger = openSessionLogger

// SessionLogPath exposes sessionLogPath for testing.
var SessionLogPath = sessionLogPath

// `wrapText` and `visibleWidth` lived in the pty rendering files
// (panel.go) deleted by #419; their tests die with them.
