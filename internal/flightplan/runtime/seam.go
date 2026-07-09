package runtime

import (
	"context"
	"fmt"
)

// runSeam executes a marked llm-seam step (a plan may declare more than one,
// #2100). It is the ONLY place the runtime reaches the LLMSeam interface. When
// no seam provider is configured (the v1 default) on the non-suspendable path,
// it errors with a clear "no seam provider configured" message, so a default
// launch reaches no LLM at all and the no-LLM guarantee holds by default, not
// merely by construction. On the suspendable path an unfulfilled seam suspends
// the run before reaching this function (see executor.suspendFor).
//
// Keeping seam invocation in this dedicated function, distinct from the
// transform and action-call paths, is the structural guarantee: the
// deterministic branches never call runSeam and never touch the LLMSeam type.
func runSeam(ctx context.Context, seam LLMSeam, step Step, bindings map[string]any) (map[string]any, error) {
	if seam == nil {
		return nil, fmt.Errorf("flightplan: step %q is an llm-seam but no seam provider is configured (v1 leaves the seam unwired by default)", step.ID)
	}
	out, err := seam.Run(ctx, SeamRequest{
		StepID:   step.ID,
		Bindings: bindings,
		Outputs:  step.Outputs,
		Prompt:   step.Prompt,
		Model:    step.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("flightplan: llm-seam step %q failed: %w", step.ID, err)
	}
	// The seam must produce every declared output so downstream bindings
	// resolve. A missing output is a hard error: the graph cannot proceed.
	for _, name := range step.Outputs {
		if _, ok := out[name]; !ok {
			return nil, fmt.Errorf("flightplan: llm-seam step %q did not produce declared output %q", step.ID, name)
		}
	}
	return out, nil
}
