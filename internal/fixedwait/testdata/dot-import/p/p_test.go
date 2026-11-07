package p

import (
	"testing"
	. "time"
)

type pause struct{ Sleep Duration }

func TestDotted(t *testing.T) {
	_ = pause{Sleep: Millisecond}
	Sleep(Millisecond)
}
