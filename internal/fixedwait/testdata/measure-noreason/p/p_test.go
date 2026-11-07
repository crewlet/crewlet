package p

import (
	"testing"
	"time"
)

func TestMarked(t *testing.T) {
	start := time.Now()
	//fixedwait:measure
	t.Logf("took %v", time.Since(start))
}
