package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"

	"github.com/ALRubinger/aileron/internal/flightplan/resolver"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

const skillUsage = `usage:
  aileron skill install <source>   Install a skill from a local path or git URL into ~/.aileron/skills
  aileron skill list               List installed skills
  aileron skill freeze <name>      Seal an installed skill into a signed, digest-pinned Flight Plan version
  aileron skill launch <name>      Run a frozen Flight Plan deterministically (no-LLM step graph)`

// skillStoreDir is a seam so tests can point the CLI at a temp store
// instead of the user's real ~/.aileron/skills.
var skillStoreDir = ""

// newSkillFetcher builds the resolver fetcher that reaches the daemon's
// GET /v1/actions. It is a package-level seam so tests can inject a fake
// without a live daemon. A nil return means "no daemon reachable"; the
// installer then degrades (records refs unsatisfied) rather than failing.
var newSkillFetcher = func() (resolver.Fetcher, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return nil, err
	}
	return resolver.HTTPFetcher{
		BaseURL:   base,
		Client:    &http.Client{Timeout: actionsHTTPClient.Timeout},
		Authorize: setDaemonAuthorization,
	}, nil
}

func runSkill(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
	switch args[0] {
	case "install":
		return runSkillInstall(args[1:], stdout, stderr)
	case "list":
		return runSkillList(args[1:], stdout, stderr)
	case "freeze":
		return runSkillFreeze(args[1:], stdout, stderr)
	case "launch":
		return runSkillLaunch(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skill command: %q\n", args[0])
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
}

func runSkillInstall(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skill install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
	src := flags.Arg(0)

	s := store.New(skillStoreDir)

	// Parse first (cheaply, by installing) — but we only want to consult the
	// daemon for skills that declare requires. The store's Install handles
	// the short-circuit: it parses, and reaches the fetcher ONLY when the
	// skill declares action refs. We pass a lazily-resolved fetcher so an
	// instruction-only install never even constructs a daemon client.
	opts := store.InstallOptions{Fetcher: lazyFetcher{stderr: stderr}}

	res, err := s.Install(context.Background(), src, opts)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if res.Degraded() {
		fmt.Fprintf(stderr, "warning: skill %q installed, but %d action requirement(s) are not satisfiable here:\n",
			res.Name, len(res.Unsatisfied))
		for _, ref := range res.Unsatisfied {
			fmt.Fprintf(stderr, "  - %s\n", ref)
		}
		fmt.Fprintln(stderr, "  Install the connectors/actions these refs name, or launch where they are available.")
	}

	fmt.Fprintf(stdout, "Installed skill %q to %s\n", res.Name, res.Dir)
	return 0
}

func runSkillList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skill list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 1
	}

	s := store.New(skillStoreDir)
	names, err := s.List()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(names) == 0 {
		fmt.Fprintln(stdout, "No skills installed.")
		fmt.Fprintln(stdout, "Run `aileron skill install <source>` to install one.")
		return 0
	}
	for _, n := range names {
		fmt.Fprintln(stdout, n)
	}
	return 0
}

// lazyFetcher defers daemon-client construction until the store actually
// needs to resolve requires. The store calls FetchActions only for skills
// that declare action refs, so an instruction-only install never reaches
// here and never touches the daemon (P2 isolation). If the daemon cannot
// be reached when resolution IS needed, it warns and returns no actions so
// the install degrades (warn) rather than failing.
type lazyFetcher struct {
	stderr io.Writer
}

func (l lazyFetcher) FetchActions(ctx context.Context) ([]resolver.Action, error) {
	f, err := newSkillFetcher()
	if err != nil {
		fmt.Fprintf(l.stderr, "warning: could not reach the daemon to resolve action requirements: %v\n", err)
		return nil, nil
	}
	actions, err := f.FetchActions(ctx)
	if err != nil {
		fmt.Fprintf(l.stderr, "warning: could not list actions to resolve requirements: %v\n", err)
		return nil, nil
	}
	return actions, nil
}
