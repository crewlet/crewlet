//go:build elsewhere

package p

import (
	"testing"
	"time"
)

func TestElsewhere(t *testing.T) {
	start := time.Now()
	_ = time.Now().Sub(start)
}
