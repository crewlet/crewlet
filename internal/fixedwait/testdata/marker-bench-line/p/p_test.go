package p

import (
	"testing"
	"time"
)

func BenchmarkMarked(b *testing.B) {
	for b.Loop() {
		time.Sleep(time.Millisecond) //fixedwait:bench
	}
}
