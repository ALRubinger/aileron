package action

import (
	"bufio"
	"bytes"
	"strings"

	"github.com/BurntSushi/toml"
)

// frontmatterDelim is the literal token that opens and closes the TOML
// frontmatter block, per ADR-0001.
const frontmatterDelim = "+++"

// Parse splits the +++-delimited TOML frontmatter from the Markdown body and
// returns the populated Manifest. `file` is the path to the source file (used
// for structured error reporting); `data` is the file contents.
//
// The contract:
//   - The file MUST begin with a `+++` line (after optional leading blank
//     lines or BOMs are tolerated).
//   - The frontmatter block MUST close with a line containing only `+++`.
//   - Everything between is parsed as TOML.
//   - Everything after is captured verbatim as the Markdown body.
//
// Any deviation surfaces as a ClassParseError carrying the offending file and
// (when known) line. Validation of required fields is performed by Validate.
func Parse(file string, data []byte) (*Manifest, error) {
	frontmatter, body, fmStart, err := splitFrontmatter(file, data)
	if err != nil {
		return nil, err
	}

	m := &Manifest{}
	meta, decErr := toml.Decode(frontmatter, m)
	if decErr != nil {
		// BurntSushi reports line numbers relative to the frontmatter
		// substring; offset by the frontmatter's start line so the error
		// points at the actual file line.
		line := tomlErrorLine(decErr) + fmStart
		return nil, newParseErr(file, line, "invalid TOML frontmatter: %s", decErr.Error())
	}

	// Reject unknown frontmatter keys: the schema is closed in v1, and a
	// silently-ignored key in a security-relevant manifest is exactly the
	// kind of footgun ADR-0001 chose TOML to avoid.
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, newParseErr(file, fmStart, "unknown frontmatter key(s): %s", strings.Join(keys, ", "))
	}

	m.Body = body
	return m, nil
}

// splitFrontmatter walks the input line-by-line, locates the opening and
// closing `+++` delimiters, and returns the frontmatter (between the
// delimiters), the Markdown body (after the closing delimiter), and the line
// number of the first frontmatter content line (1-based — used to translate
// TOML decoder line numbers to file line numbers).
func splitFrontmatter(file string, data []byte) (frontmatter, body string, fmStart int, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Accommodate longer lines than the default 64KB.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		opened     bool
		closed     bool
		fmLines    []string
		bodyLines  []string
		lineNo     int
		closeLine  int
	)

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()

		// Tolerate BOM on the first line.
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}

		switch {
		case !opened:
			// Lines before the opening delimiter must be blank — anything
			// else is a parse error so authors can't accidentally paste
			// content above the manifest.
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if trimmed != frontmatterDelim {
				return "", "", 0, newParseErr(file, lineNo, "expected %q at start of file, got %q", frontmatterDelim, trimmed)
			}
			opened = true
			fmStart = lineNo + 1

		case opened && !closed:
			if strings.TrimSpace(line) == frontmatterDelim {
				closed = true
				closeLine = lineNo
				continue
			}
			fmLines = append(fmLines, line)

		default:
			bodyLines = append(bodyLines, line)
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		return "", "", 0, newParseErr(file, lineNo, "read error: %s", scanErr.Error())
	}

	if !opened {
		return "", "", 0, newParseErr(file, 0, "missing %q opening delimiter", frontmatterDelim)
	}
	if !closed {
		return "", "", 0, newParseErr(file, fmStart, "missing %q closing delimiter", frontmatterDelim)
	}
	_ = closeLine // currently unused; reserved for richer diagnostics.

	frontmatter = strings.Join(fmLines, "\n")
	if len(bodyLines) > 0 {
		body = strings.Join(bodyLines, "\n")
	}
	return frontmatter, body, fmStart, nil
}

// tomlErrorLine extracts the line number from a BurntSushi/toml error when
// the error is a *toml.ParseError. Returns 0 when the underlying type does
// not expose a line.
func tomlErrorLine(err error) int {
	type lineError interface {
		Line() int
	}
	if le, ok := err.(lineError); ok && le.Line() > 0 {
		return le.Line() - 1 // BurntSushi lines are 1-based within the frontmatter; we add fmStart (also 1-based) so subtract 1
	}
	// Fall back to parsing "Near line N" prefixes some versions emit.
	msg := err.Error()
	const prefix = "Near line "
	if i := strings.Index(msg, prefix); i >= 0 {
		rest := msg[i+len(prefix):]
		var n int
		for _, r := range rest {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		if n > 0 {
			return n - 1
		}
	}
	return 0
}
