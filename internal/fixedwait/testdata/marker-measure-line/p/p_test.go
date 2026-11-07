package p

import (
	"testing"
	"time"
)

func TestMarked(t *testing.T) {
	start := time.Now()
	t.Logf("took %v", time.Since(start)) //fixedwait:measure logged for the run's report
}
