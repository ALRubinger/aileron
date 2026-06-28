package main

import "flag"

// parseInterspersedFlags parses args into the given FlagSet while allowing
// flags to appear before OR after positional arguments.
//
// Go's stdlib flag.Parse stops scanning at the first non-flag token, so a
// command like `aileron action run my-action --arg k=v` leaves `--arg k=v`
// unparsed: flag.Parse treats `my-action` as the first positional and never
// looks past it. That makes the natural `<positional> --flag` ordering fail
// while the unnatural `--flag <positional>` ordering works, an inconsistency
// users hit repeatedly.
//
// This helper restores interspersed parsing by calling fs.Parse repeatedly:
// each pass consumes the flags up to the next positional, that positional is
// peeled off, and the remainder is re-parsed. The collected positionals are
// returned in their original left-to-right order. Callers should read
// positionals from the returned slice rather than fs.Arg/fs.NArg, since the
// FlagSet only retains the final pass's residual args.
//
// Repeatable flags registered with fs.Var (for example a `--arg` that
// accumulates) work across passes because each pass calls Set for the flags
// it sees, appending to the same destination. Scalar flags keep the last
// value set, and a flag absent from a given pass retains its prior value.
//
// A `--` terminator is honored by the underlying flag.Parse: tokens after it
// are treated as positionals on the pass that encounters it.
func parseInterspersedFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}
