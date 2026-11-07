package p

import (
	"testing"
	"time"
)

func TestMarked(t *testing.T) {
	start := time.Now()
	//fixedwait:measure logged for the run's report
	time.Sleep(time.Millisecond)
	//fixedwait:liveness
	t.Logf("took %v", time.Since(start))
	time.Sleep(time.Millisecond)    //fixedwait:liveness
	_ = time.Since(start)           //fixedwait:measure logged for the run's report
	<-time.After(time.Since(start)) //fixedwait:measure logged for the run's report
}
