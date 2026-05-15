//go:build windows

package sandbox

import "fmt"

// checkPlatformSandboxAvailable probes the Windows sandbox
// primitives: opens a session to the Windows Filtering Platform
// engine (validates that the Base Filtering Engine service is
// running and the daemon has access rights). Close on success.
//
// The restricted-token + job-object pieces from Windows v1
// don't have a meaningful pre-spawn probe — they always
// succeed on a healthy Windows install — so the WFP open is
// the load-bearing check. A host where WFP is unreachable
// would also reject the runtime's filter add at spawn time,
// but failing at install time gives the operator a clearer
// remediation step.
func checkPlatformSandboxAvailable() error {
	engine, err := openWFPEngine()
	if err != nil {
		return fmt.Errorf("%w: WFP engine probe: %v", ErrSpawnUnavailable, err)
	}
	_ = closeWFPEngine(engine)
	return nil
}
