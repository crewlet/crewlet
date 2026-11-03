package code_test

import (
	"bytes"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// BenchmarkClassifyGo is what ONE PATH COSTS a classification at the bound:
// two versions of a file MaxClassedBlobBytes long, classed against each other
// to the end — the versions are alike, so nothing cuts the walk short — in
// the shapes that set what a byte costs:
//
//   - code: ordinary Go, one function after another, a comment beside a
//     statement now and then;
//   - comments: nothing but comments, one to a line — the most lexemes a file
//     of the bound holds, every one of them a comment;
//   - governed: the same comments below a //go:noinline, so that every one is
//     what the directive governs and is compared whole.
//
// MaxClassedBlobBytes' doc records the figures.
func BenchmarkClassifyGo(b *testing.B) {
	for _, arm := range []struct{ name, head, fill, tail string }{
		{"code", "package p\n\n", "func f() int {\n\tx := 1\n\tfor i := 0; i < x; i++ {\n\t\tx += i // one at a time\n\t}\n\treturn x\n}\n\n", ""},
		{"comments", "package p\n\n", "//\n", "func f() {}\n"},
		{"governed", "package p\n\n//go:noinline\n", "//\n", "func f() {}\n"},
	} {
		before := filled(arm.head, arm.fill, arm.tail, code.MaxClassedBlobBytes)
		after := bytes.Clone(before)
		b.Run(arm.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(before) + len(after)))
			for b.Loop() {
				if class, ok := code.ClassifyGo(before, after); !ok || class != code.ChangeClassWhitespaceOnly {
					b.Fatalf("classed %q (%v), want whitespace_only", class, ok)
				}
			}
		})
	}
}

// filled is a Go file of fill, repeated as often as size bytes allow between
// head and tail.
func filled(head, fill, tail string, size int) []byte {
	var src bytes.Buffer
	src.WriteString(head)
	for src.Len()+len(fill)+len(tail) <= size {
		src.WriteString(fill)
	}
	src.WriteString(tail)
	return src.Bytes()
}
