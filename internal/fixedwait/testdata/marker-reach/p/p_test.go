package p

import (
	"testing"
	"time"
)

func TestATrailingMarkerCoversItsOwnLineAlone(t *testing.T) {
	<-time.After(time.Millisecond) //fixedwait:liveness
	time.Sleep(time.Millisecond)
}

func TestAMarkerAloneCoversTheLineUnderItAlone(t *testing.T) {
	//fixedwait:liveness
	t.Log("between the marker and the wait")
	time.Sleep(time.Millisecond)
}
