package approval

import (
	"github.com/ALRubinger/aileron/internal/store/mem"
)

func newTestApprovalStore() *mem.ApprovalStore {
	return mem.NewApprovalStore()
}
