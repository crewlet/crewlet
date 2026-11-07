package p

import (
	"testing"
	"testing/synctest"
)

// settle waits for nothing.
//
//fixedwait:bubble
func settle() {}

func TestSettles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) { settle() })
}
