package freeze

import (
	"fmt"
	"strings"
)

// sealStepTrust walks the manifest's step graph and seals each tool step's
// declared trust-contract reach into the step-keyed lock section
// (`lock.stepTrust`). Hosts only: the full contract stays in the signed
// frontmatter bytes; this section is the resolved reach launch/runtime
// consume, covered by the content hash and signature so it cannot be
// re-supplied at launch.
//
// Selection is fail-closed:
//
//   - only `kind: tool` steps participate; a tool step with no declared
//     trustContract is omitted (it declares no reach);
//   - a declared trustContract that is not a mapping, or whose hosts are
//     missing, empty, or carry a non-string/empty entry, is a hard error —
//     never a partial seal (the schema rejects these shapes before freeze
//     runs; this is the freeze-time defensive backstop);
//   - a contracted tool step with a missing/empty id, or two contracted tool
//     steps sharing an id, is a hard error: the id is the lock key the
//     runtime dispatches on, so an ambiguous key must never be sealed.
//
// An empty result returns nil so the lock section is omitted entirely
// (omitempty) and a no-tool-steps plan's lock bytes are unchanged.
func sealStepTrust(steps []any) (map[string]StepReach, error) {
	var out map[string]StepReach
	for i, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			// Lint (which runs before sealing) already rejects non-mapping
			// steps; keep the backstop for direct constructs.
			return nil, fmt.Errorf("freeze: step %d is not a mapping", i)
		}
		if kind, _ := step["kind"].(string); kind != "tool" {
			continue
		}
		hosts, declared, err := toolStepHosts(step, i)
		if err != nil {
			return nil, err
		}
		if !declared {
			continue
		}
		id, _ := step["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("freeze: tool step %d declares a trustContract but no id (the id keys the sealed reach)", i)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("freeze: duplicate tool step id %q (each sealed reach needs a unique key)", id)
		}
		if out == nil {
			out = make(map[string]StepReach)
		}
		out[id] = StepReach{Hosts: hosts}
	}
	return out, nil
}

// toolStepHosts extracts the declared trust-contract hosts from a tool step's
// untyped map. declared is false when the step carries no trustContract (an
// optional block: absent means the step declares no reach). A declared
// trustContract whose hosts are missing, empty, or carry a non-string/empty
// entry is rejected rather than sealing a partial reach, so a malformed reach
// never silently produces a wrong seal.
func toolStepHosts(step map[string]any, index int) (hosts []string, declared bool, err error) {
	raw, ok := step["trustContract"]
	if !ok || raw == nil {
		return nil, false, nil
	}
	tc, ok := raw.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("freeze: tool step %d has a trustContract that is not a mapping", index)
	}
	hostsRaw, ok := tc["hosts"]
	if !ok || hostsRaw == nil {
		return nil, false, fmt.Errorf("freeze: tool step %d declares a trustContract with no hosts (access scope is the boundary)", index)
	}
	list, ok := hostsRaw.([]any)
	if !ok || len(list) == 0 {
		return nil, false, fmt.Errorf("freeze: tool step %d declares a trustContract with an empty or non-array hosts", index)
	}
	hosts = make([]string, 0, len(list))
	for _, h := range list {
		s, ok := h.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, false, fmt.Errorf("freeze: tool step %d declares a trustContract host that is empty or not a string", index)
		}
		// The trimmed host string is sealed VERBATIM: a host TEMPLATE carrying
		// `{{ inputs.<name> }}` tokens (#1959) rides into lock.stepTrust exactly
		// as authored, covered by the content hash and detached signature. No
		// host-shape validation happens here on purpose — the token grammar and
		// its constraint-presence guard live in lintHostInterpolation, and the
		// runtime instantiates the sealed template into a concrete host before
		// the step-scope mint. Sealing the template (not a pre-instantiated
		// host) is what makes the token bytes part of the attestation.
		hosts = append(hosts, strings.TrimSpace(s))
	}
	return hosts, true, nil
}
