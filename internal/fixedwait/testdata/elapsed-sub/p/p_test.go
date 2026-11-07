package p

import (
	"testing"
	"time"
)

func TestThroughAValue(t *testing.T) {
	start := time.Now()
	if time.Now().Sub(start) > time.Second {
		t.Fatal("slow")
	}
}

func TestThroughAnAlias(t *testing.T) {
	var a, b stamp
	_ = a.Sub(b)
}

func TestThroughAnEmbedding(t *testing.T) {
	var s stamped
	_ = s.Sub(time.Time{})
}

func TestThroughAConversion(t *testing.T) {
	var i instant
	_ = time.Time(i).Sub(time.Time{})
}

func TestAsAValue(t *testing.T) {
	sub := time.Time.Sub
	_ = sub
}

func TestThroughAMethodValue(t *testing.T) {
	start := time.Now()
	since := start.Sub
	_ = since(time.Now())
}

func TestThroughAPointer(t *testing.T) {
	start := time.Now()
	at := &start
	_ = at.Sub(time.Now())
	var h struct{ began *time.Time }
	h.began = at
	_ = h.began.Sub(time.Now())
}
