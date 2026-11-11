//go:build elsewhere

package p

import (
	"testing"
	"time"
)

func TestElsewhereInTime(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	if !time.Now().Before(deadline) {
		t.Fatal("late")
	}
}
