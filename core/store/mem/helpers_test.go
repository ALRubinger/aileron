package mem_test

import "github.com/ALRubinger/aileron/core/store"

func isErrNotFound(err error, target **store.ErrNotFound) bool {
	if err == nil {
		return false
	}
	var nf *store.ErrNotFound
	ok := false
	if e, isNF := err.(*store.ErrNotFound); isNF {
		nf = e
		ok = true
	}
	if ok && target != nil {
		*target = nf
	}
	return ok
}
