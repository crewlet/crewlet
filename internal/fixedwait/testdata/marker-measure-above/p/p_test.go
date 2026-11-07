package p

import (
	"testing"
	"time"
)

func TestMarked(t *testing.T) {
	start := time.Now()
	//fixedwait:measure logged for the run's report
	t.Logf("took %v", time.Now().Sub(start))
}
