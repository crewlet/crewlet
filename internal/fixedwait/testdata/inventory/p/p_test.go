package p

import (
	"testing"
	"time"
)

func TestTwoWaits(t *testing.T) {
	time.Sleep(time.Millisecond)
	<-time.After(time.Millisecond)
}

type suite struct{}

func (*suite) waitABit() {
	time.Sleep(time.Millisecond)
}

func TestNoWait(t *testing.T) {}
