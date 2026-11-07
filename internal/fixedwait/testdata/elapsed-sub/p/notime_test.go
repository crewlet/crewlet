package p

import "testing"

func TestWithoutTheTimeImport(t *testing.T) {
	var e event
	_ = e.at.Sub(e.from)
}
