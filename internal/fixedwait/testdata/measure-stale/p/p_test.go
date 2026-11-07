package p

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestMarked(t *testing.T) {
	//fixedwait:measure logged for the run's report
	t.Log("nothing is read here")

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		//fixedwait:measure logged for the run's report
		t.Logf("took %v", time.Since(start))
	})
}
