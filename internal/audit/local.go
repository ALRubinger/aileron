package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ShellEntry is a single audit record for a shell command decision.
// Written as one JSONL line per entry by aileron-sh.
type ShellEntry struct {
	Timestamp   time.Time `json:"timestamp"`
	SessionID   string    `json:"session_id"`
	Command     string    `json:"command"`
	Disposition string    `json:"disposition"` // allow, deny, ask_approved, ask_denied
	RuleID      string    `json:"rule_id"`     // which policy rule matched
	Agent       string    `json:"agent"`
}

// AppendShellEntry appends one audit entry to the JSONL log file.
// Uses O_APPEND for atomic writes (safe for concurrent aileron-sh processes
// as long as each write is under PIPE_BUF).
func AppendShellEntry(path string, entry ShellEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating audit log directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening audit log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encoding audit entry: %w", err)
	}
	data = append(data, '\n')

	_, err = f.Write(data)
	return err
}

// ReadShellEntries reads all JSONL entries from the audit log.
// Malformed lines are silently skipped.
func ReadShellEntries(path string) ([]ShellEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []ShellEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var entry ShellEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // skip malformed lines
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

// ShellFilter specifies criteria for filtering audit entries.
type ShellFilter struct {
	SessionID      string
	Disposition    string
	CommandPattern string // substring match
}

// MessageEntry is a single audit record for a message event.
type MessageEntry struct {
	Timestamp   time.Time `json:"timestamp"`
	SessionID   string    `json:"session_id"`
	Event       string    `json:"event"`                 // message_received, message_sent, message_denied, message_dismissed, draft_requested, reply_sent
	Service     string    `json:"service"`               // slack, discord
	Channel     string    `json:"channel"`
	Author      string    `json:"author,omitempty"`      // sender for inbound messages
	Body        string    `json:"body,omitempty"`        // message content
	InReplyTo   string    `json:"in_reply_to,omitempty"` // original message ID
}

// AppendMessageEntry appends one message audit entry to the JSONL log file.
func AppendMessageEntry(path string, entry MessageEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating audit log directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening audit log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encoding audit entry: %w", err)
	}
	data = append(data, '\n')

	_, err = f.Write(data)
	return err
}

// ReadMessageEntries reads all message JSONL entries from the audit log.
// Only entries with an "event" field are returned (shell entries are skipped).
func ReadMessageEntries(path string) ([]MessageEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []MessageEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var entry MessageEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Event == "" {
			continue // skip shell entries
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

// ReadShellEntriesFiltered reads entries matching the given filter.
func ReadShellEntriesFiltered(path string, filter ShellFilter) ([]ShellEntry, error) {
	all, err := ReadShellEntries(path)
	if err != nil {
		return nil, err
	}

	var filtered []ShellEntry
	for _, e := range all {
		if filter.SessionID != "" && e.SessionID != filter.SessionID {
			continue
		}
		if filter.Disposition != "" && e.Disposition != filter.Disposition {
			continue
		}
		if filter.CommandPattern != "" && !strings.Contains(e.Command, filter.CommandPattern) {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered, nil
}
