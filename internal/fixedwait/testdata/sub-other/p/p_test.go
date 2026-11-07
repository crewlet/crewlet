package p

import (
	"math/big"
	"testing"
	"time"
)

func TestOtherSubs(t *testing.T) {
	var a, b big.Int
	a.Sub(&a, &b)
	_ = set{}.Sub(set{})
	_ = instant(time.Now()).Sub(instant{})
}
