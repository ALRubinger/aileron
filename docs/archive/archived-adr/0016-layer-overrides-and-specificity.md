---
title: "ADR-0016: Layer Overrides and Pattern Specificity"
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-10</td></tr>
</table>
</div>

## Context

The priority point system (allow=50, ask=100, deny=200) introduced in ADR-0014 and ADR-0015 is overengineered. Users have to think about numeric priorities when they should just think about which layer they're writing rules in and how specific their patterns are.

The three-layer model (defaults, user, project) needs a simpler conflict resolution mechanism.

## Decision

Replace the priority point system with two rules:

### 1. Later layer wins for the same pattern

When merging layers (defaults -> user -> project), if the overlay has a rule with the same command pattern as a base rule, the base rule is **replaced**. The overlay's bucket (allow/deny/ask) determines the effect.

```
Default: deny  "security *"
Project: allow "security *"
-> Only the project allow survives. The default deny is replaced.
```

### 2. More specific pattern wins regardless of layer

When multiple rules match a command at eval time, the most specific pattern wins. Specificity is measured by counting non-wildcard characters in the pattern.

```
Default: allow "git *"           (specificity: 4)
Project: deny  "git push origin main"  (specificity: 20)
-> Both survive merge (different patterns). At eval, the deny wins
   because it has higher specificity.
```

For equal specificity, a safety-biased tie-breaker applies: deny > ask > allow. This is implemented as a small offset added to the specificity score (deny+2, ask+1, allow+0), multiplied by 3 to create spacing.

### Implementation

- The `Priority` field is removed from the `Rule` struct. Users no longer assign numeric priorities.
- `Merge()` drops base rules whose command pattern appears in the overlay (any bucket).
- `ToEngineRules()` computes `PatternSpecificity(pattern) * 3 + effectOffset` as the engine priority. The existing engine sorts by priority descending, so no engine changes are needed.
- Rule IDs are tagged with layer prefixes (`default:`, `user:`, `project:`) for auditability.
- `BuiltinAskRules()` use the same specificity-based priority as all other rules.

### Examples

| Scenario | Default | Project | Result |
|----------|---------|---------|--------|
| Project allows what default denies | deny "security *" | allow "security *" | allow (project replaces default) |
| Specific deny beats general allow | allow "git *" | deny "git push origin main" | deny wins for "git push origin main", allow wins for "git status" |
| User overrides project | (project) deny "docker push *" | (user) allow "docker push *" | allow (user replaces project) |

## Consequences

- Users can override any default rule by defining the same pattern in their project or user config.
- More specific patterns always win over general ones, regardless of which layer defined them.
- The `priority` field in aileron.yaml is no longer supported. Existing files with `priority:` will have the field silently ignored (it's removed from the schema).
- Built-in ask rules (pipe-to-shell, eval, etc.) can be overridden by project or user policy like any other default rule.
