package p_test

import (
	"testing"
	"time"

	"example.com/fixture/p"
)

func TestFromTheExternalPackage(t *testing.T) {
	_ = time.Now().Sub(p.Started)
}
