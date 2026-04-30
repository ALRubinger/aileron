package action

// EnforceCapability checks whether the given action manifest permits the
// supplied connector operation under its declared capability subset, per
// ADR-0003.
//
// The action boundary is the second of the two defense-in-depth checks
// (the connector manifest is the first). A call that requires a capability
// outside the action's declared subset is denied here before it ever reaches
// the connector. Returns nil on permitted calls; on denial returns a
// structured *Error of class capability_denied with enough context for the
// caller to surface a precise message to the agent and audit log.
//
// `connectorFQN` is the connector's fully-qualified URI (e.g.
// "github://aileron/slack"); it must match one of the manifest's
// [[requires.connectors]] entries exactly. `capability` is the operation the
// caller is attempting (e.g. "chat:write").
func EnforceCapability(m *Manifest, connectorFQN, capability string) error {
	if m == nil {
		return &Error{
			Class:    ClassCapabilityDenied,
			Boundary: BoundaryAction,
			Message:  "no action manifest in scope",
			Details: map[string]any{
				"connector":  connectorFQN,
				"capability": capability,
			},
		}
	}

	for _, c := range m.Requires.Connectors {
		if c.Name != connectorFQN {
			continue
		}
		for _, declared := range c.Capabilities {
			if declared == capability {
				return nil
			}
		}
		return &Error{
			Class:    ClassCapabilityDenied,
			Boundary: BoundaryAction,
			Message:  "capability not in declared subset",
			Details: map[string]any{
				"action":           m.Name,
				"action_version":   m.Version,
				"connector":        connectorFQN,
				"connector_version": c.Version,
				"requested":        capability,
				"declared_subset":  append([]string(nil), c.Capabilities...),
			},
		}
	}

	return &Error{
		Class:    ClassCapabilityDenied,
		Boundary: BoundaryAction,
		Message:  "connector not declared as a dependency",
		Details: map[string]any{
			"action":         m.Name,
			"action_version": m.Version,
			"connector":      connectorFQN,
			"requested":      capability,
		},
	}
}
