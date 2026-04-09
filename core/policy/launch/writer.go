package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppendAllowRule appends a command pattern to the allow list of a YAML
// policy file. If the file has an existing `allow:` block, the new rule
// is inserted after the last entry in that block. If no `allow:` block
// exists, one is appended at the end of the file. If the file does not
// exist, it is created with a minimal valid structure.
//
// This function manipulates the raw YAML text rather than parsing and
// rewriting, which preserves the user's existing formatting and comments.
func AppendAllowRule(path, pattern string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}

	entry := fmt.Sprintf("  - %q", pattern)

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		content := fmt.Sprintf("version: 1\nallow:\n%s\n", entry)
		return os.WriteFile(path, []byte(content), 0o644)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	updated, inserted := insertAllowEntry(lines, entry)
	if !inserted {
		// No allow: block found — append one.
		updated = append(updated, "allow:", entry, "")
	}

	return os.WriteFile(path, []byte(strings.Join(updated, "\n")), 0o644)
}

// insertAllowEntry scans for an `allow:` block and inserts the entry
// after the last `  -` line in that block. Returns the updated lines
// and whether an insertion was made.
func insertAllowEntry(lines []string, entry string) ([]string, bool) {
	allowIdx := -1
	lastEntryIdx := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "allow:" {
			allowIdx = i
			lastEntryIdx = i
			continue
		}
		// Inside the allow block: track lines that are list entries.
		if allowIdx >= 0 && i > allowIdx {
			if strings.HasPrefix(line, "  -") {
				lastEntryIdx = i
			} else if trimmed == "" {
				// Blank lines inside the block are OK, keep scanning.
				continue
			} else {
				// Hit a non-entry, non-blank line → block is over.
				break
			}
		}
	}

	if allowIdx < 0 {
		return lines, false
	}

	// Insert after lastEntryIdx.
	result := make([]string, 0, len(lines)+1)
	result = append(result, lines[:lastEntryIdx+1]...)
	result = append(result, entry)
	result = append(result, lines[lastEntryIdx+1:]...)
	return result, true
}
