package q

import "testing"

func TestWithoutTheTimeImport(t *testing.T) {
	var e event
	if e.at.After(e.from) {
		t.Fatal("moved")
	}
}
