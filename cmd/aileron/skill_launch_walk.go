package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// newLaunchInputWalker constructs the interactive input walker wired into the
// runtime on an interactive launch (#2063). It is a package-level seam so CLI
// tests inject a deterministic stdin/stdout and drive the walk without a live
// TTY, mirroring newLaunchInputPrompter. The caller (runSkillLaunch) decides
// whether to wire it: it is passed only when the launch is interactive
// (isTTYFn() && !--accept-defaults), so this constructor never re-checks the
// TTY.
var newLaunchInputWalker = func(stdin *bufio.Reader, stdout io.Writer) runtime.InputWalker {
	return launchInputWalker{stdin: stdin, stdout: stdout}
}

// launchInputWalker is the guided interactive input walk: it walks every
// declared input in declaration order, prompting once per literal input
// (name+description, required/optional marker, current default with
// Enter-to-accept, enum/pattern hint), re-prompting on an invalid entry. It
// runs host-side, before container boot, so it feeds inputs into the sealed
// (frozen-plan) mainline where the in-container prompter is never consulted.
//
// The stdin is a persistent *bufio.Reader so bytes buffered past one prompt
// remain for the next; a fresh wrapper per prompt would buffer-ahead and drop
// the rest of the input.
type launchInputWalker struct {
	stdin  *bufio.Reader
	stdout io.Writer
}

// maxDefaultInlineLen bounds the string form of a default rendered inline in a
// prompt. A default whose string form is larger renders as the same type+size
// summary the result banner uses (#1888), so a big default never floods the
// terminal.
const maxDefaultInlineLen = 256

// Walk resolves every declared literal input into the returned launch args. It
// starts from the caller's args (the `--input` overrides), leaves those inputs
// untouched (rendering an "already set" line), prompts for every other literal,
// and renders a read-only informational line for dynamic and source inputs
// (never editable). The returned args carry a value only for a literal the
// operator TYPED (entered as a string); an Enter-accepted default is left unset,
// so the declared default resolves natively downstream (see the typing note in
// the loop). On EOF or a read error mid-walk it returns the error and the launch
// fails, rather than spinning on a drained reader.
func (w launchInputWalker) Walk(inputs []runtime.Input, args runtime.LaunchArgs) (runtime.LaunchArgs, error) {
	out := runtime.LaunchArgs{}
	for k, v := range args {
		out[k] = v
	}
	for _, in := range inputs {
		if in.Resolution.Rule != runtime.ResolutionLiteral {
			// Dynamic and source inputs resolve automatically at the boundary;
			// render a read-only line and never prompt for them.
			fmt.Fprintln(w.stdout, w.readOnlyLine(in))
			continue
		}
		if _, ok := out[in.Name]; ok {
			// A literal already supplied via --input keeps its value; show it as
			// already set and skip the prompt so an override is never re-asked.
			// This check sits BEFORE the NoPrompt skip so a `prompt: false` input
			// stays --input-overridable: an explicit override still wins.
			fmt.Fprintln(w.stdout, w.alreadySetLine(in))
			continue
		}
		if in.NoPrompt {
			// An advanced/non-interactive input (`prompt: false`) is skipped: its
			// declared default applies silently downstream. Nothing is written into
			// out[in.Name], so resolveInputs sees no override and applies the
			// default. Rendered as an informational line so the walk stays legible.
			fmt.Fprintln(w.stdout, w.advancedLine(in))
			continue
		}
		val, typed, err := w.walkLiteral(in)
		if err != nil {
			return nil, err
		}
		// Launch-mode parity for an accepted default (#2063). An operator-typed
		// entry is written into out as a string override. An Enter-accepted
		// default is NOT written: leaving out[in.Name] unset makes resolveInputs
		// apply the declared default NATIVELY downstream, exactly as the
		// NoPrompt/advanced skip above and --accept-defaults do. Serializing the
		// accepted default into the args instead would round-trip it through the
		// sealed-image `--input` re-entry as a string, so an interactive Enter
		// would diverge from the native-typed value --accept-defaults resolves on
		// the same image path. Not serializing it keeps both paths identical.
		if typed {
			out[in.Name] = val
		}
	}
	return out, nil
}

// walkLiteral prompts for one literal input, re-prompting until it reads a valid
// value or an Enter-accepted default. It returns (validatedTypedString, true,
// nil) on a non-empty entry, (nil, false, nil) on an Enter keypress that accepts
// a declared default (the caller then leaves the arg unset so the default
// resolves natively downstream), and (nil, false, err) on EOF/read failure.
func (w launchInputWalker) walkLiteral(in runtime.Input) (any, bool, error) {
	prompt := w.literalPrompt(in)
	for {
		line, err := readPromptLine(w.stdin, w.stdout, prompt)
		if err != nil {
			return nil, false, fmt.Errorf("input %q: read value: %w", in.Name, err)
		}
		if line == "" {
			if in.Resolution.HasDefault {
				// Enter accepts the declared default. It is NOT returned as a typed
				// value: the caller leaves the arg unset so the default resolves
				// natively downstream, keeping the image path in parity with
				// --accept-defaults (#2063).
				return nil, false, nil
			}
			// Empty entry on a required-no-default literal: a value is required,
			// so re-prompt. An empty string can never be entered for such an
			// input; this routing is intentional (#2063). EOF is distinct from
			// Enter (readPromptLine returns an error for EOF), so this does not
			// spin on a drained reader.
			fmt.Fprintln(w.stdout, "  a value is required")
			continue
		}
		// Validate a typed entry against the declared constraint using the SAME
		// authoritative pass the final resolveInputs check runs (EnforceConstraint,
		// #2063), so the walk and the final pass can never disagree. An
		// unconstrained input (nil Constraint) is accepted as-is, matching
		// resolveInputs, which skips the enforcement pass for it.
		if in.Constraint != nil {
			if err := runtime.EnforceConstraint(in.Name, line, in.Constraint); err != nil {
				fmt.Fprintf(w.stdout, "  %v\n", err)
				continue
			}
		}
		return line, true, nil
	}
}

// literalPrompt renders the one-line prompt for a literal input: its name, the
// declared description, the required marker (with a "has default" note when the
// input carries one), the current default with an Enter-to-accept note
// (defaulted inputs only), and any enum/pattern hint.
func (w launchInputWalker) literalPrompt(in runtime.Input) string {
	var b strings.Builder
	b.WriteString(in.Name)
	if in.Description != "" {
		fmt.Fprintf(&b, " (%s)", in.Description)
	}
	if in.Resolution.HasDefault {
		b.WriteString(" [required · has default]")
		fmt.Fprintf(&b, " (default: %s, Enter to accept)", defaultDisplay(in.Resolution.Default))
	} else {
		b.WriteString(" [required]")
	}
	if in.HasExample {
		// A declared example is a first-class value; render it inline through the
		// same capped renderer the default uses so a large example never floods
		// the terminal.
		fmt.Fprintf(&b, " (e.g. %s)", defaultDisplay(in.Example))
	}
	if hint := inputConstraintHint(in.Constraint); hint != "" {
		fmt.Fprintf(&b, " %s", hint)
	}
	b.WriteString(": ")
	return b.String()
}

// advancedLine renders the informational line for an input marked `prompt:
// false`: it is skipped by the walk, so its declared default applies silently
// downstream. It remains overridable via --input.
func (w launchInputWalker) advancedLine(in runtime.Input) string {
	var b strings.Builder
	b.WriteString(in.Name)
	if in.Description != "" {
		fmt.Fprintf(&b, " (%s)", in.Description)
	}
	b.WriteString(" [advanced: using default, override with --input]")
	return b.String()
}

// readOnlyLine renders the informational line for a dynamic or source input: it
// is not editable, so the walk shows how it resolves and moves on.
func (w launchInputWalker) readOnlyLine(in runtime.Input) string {
	var b strings.Builder
	b.WriteString(in.Name)
	if in.Description != "" {
		fmt.Fprintf(&b, " (%s)", in.Description)
	}
	switch in.Resolution.Rule {
	case runtime.ResolutionDynamic:
		fmt.Fprintf(&b, " [resolved at launch: %s]", in.Resolution.DynamicValue)
	case runtime.ResolutionSource:
		fmt.Fprintf(&b, " [resolved from source: %s]", in.Resolution.SourceActionRef)
	default:
		b.WriteString(" [resolved automatically]")
	}
	return b.String()
}

// alreadySetLine renders the line for a literal already supplied via --input: it
// keeps its override value and is not re-prompted.
func (w launchInputWalker) alreadySetLine(in runtime.Input) string {
	var b strings.Builder
	b.WriteString(in.Name)
	if in.Description != "" {
		fmt.Fprintf(&b, " (%s)", in.Description)
	}
	b.WriteString(" [already set via --input]")
	return b.String()
}

// defaultDisplay renders a declared default for the prompt: its string form when
// short, or the type+size summary (#1888) when its string form is large, so a
// big default never floods the terminal.
func defaultDisplay(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > maxDefaultInlineLen {
		return summarizeInputValue(v)
	}
	return s
}

// readPromptLine writes prompt to stdout and reads one newline-terminated line
// from stdin, returning it (newline-stripped) and any read error. Unlike the
// shared promptLine in main.go (which collapses EOF to "" and swallows the
// error), it PROPAGATES the read error so an interactive walk terminates on a
// drained reader or a mid-walk stdin close instead of spinning: an Enter
// keypress returns ("", nil) while EOF on an empty read returns ("", err), so
// the two are distinguishable. A partial final line with content but no trailing
// newline is returned as the value (the error surfaces on the next, empty read).
//
// stdin is wrapped in a *bufio.Reader only when it isn't one already, so a
// caller that prompts more than once (the walker, the multi-prompt binding
// flows) passes a persistent *bufio.Reader and subsequent reads see the buffered
// remainder rather than a fresh buffer-ahead dropping it.
func readPromptLine(stdin io.Reader, stdout io.Writer, prompt string) (string, error) {
	fmt.Fprint(stdout, prompt)
	br, ok := stdin.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(stdin)
	}
	line, err := br.ReadString('\n')
	trimmed := strings.TrimRight(line, "\r\n")
	if err != nil && trimmed == "" {
		return "", err
	}
	return trimmed, nil
}
