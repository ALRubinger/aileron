package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// MessageEntry is a single audit record for a message event.
type MessageEntry struct {
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"session_id"`
	Event     string    `json:"event"`                 // message_received, message_sent, message_denied, message_dismissed, draft_requested, reply_sent
	Service   string    `json:"service"`               // slack, discord
	Channel   string    `json:"channel"`
	Author    string    `json:"author,omitempty"`      // sender for inbound messages
	Body      string    `json:"body,omitempty"`        // message content
	InReplyTo string    `json:"in_reply_to,omitempty"` // original message ID
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
// Only entries with an "event" field are returned (other entries are skipped).
//
// path may be either a single JSONL file or the daily-rotated directory
// `<stateDir>/audit/`. When it's a directory, every `audit-*.jsonl` file
// inside is read, sorted by name (chronological since filenames embed
// the date).
func ReadMessageEntries(path string) ([]MessageEntry, error) {
	files, err := resolveAuditFiles(path)
	if err != nil {
		return nil, err
	}
	var entries []MessageEntry
	for _, p := range files {
		got, err := readMessageEntriesFromFile(p)
		if err != nil {
			return nil, err
		}
		entries = append(entries, got...)
	}
	return entries, nil
}

func readMessageEntriesFromFile(path string) ([]MessageEntry, error) {
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
			continue
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

// resolveAuditFiles returns the list of files to read for `path`.
// If path is a directory, lists every `audit-*.jsonl` file inside,
// sorted by name. If path is a file (or does not exist), returns
// just that path.
func resolveAuditFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{path}, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if !isAuditFileName(name) {
			continue
		}
		files = append(files, filepath.Join(path, name))
	}
	sort.Strings(files)
	return files, nil
}
