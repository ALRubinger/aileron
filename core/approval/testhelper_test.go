package approval

import (
	"github.com/ALRubinger/aileron/core/store/mem"
)

func newTestApprovalStore() *mem.ApprovalStore {
	return mem.NewApprovalStore()
}
