package paced_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/paced"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// taker is a response writer that takes every byte at once, and carries a
// write deadline and a flush as net/http's own response does — so what a
// benchmark into it measures is the pacing's own cost above the bytes, with
// no connection's in the figure.
type taker struct{ header http.Header }

func (t *taker) Header() http.Header { return t.header }

func (*taker) WriteHeader(int) {}

func (*taker) Write(p []byte) (int, error) { return len(p), nil }

func (*taker) SetWriteDeadline(time.Time) error { return nil }

func (*taker) FlushError() error { return nil }

// BenchmarkSend is what a download costs above the bytes it moves: a stored
// file sent into a writer that takes it at once, at an attachment's size and
// at a multi-mebibyte one. B/op is what the piece is judged by: a piece
// allocated per download is the whole of a small file's cost.
func BenchmarkSend(b *testing.B) {
	for _, size := range []int{4 << 10, 64 << 20} {
		b.Run("size="+strconv.Itoa(size>>10)+"KiB", func(b *testing.B) {
			data := bytes.Repeat([]byte("crewlet!"), size/8)
			src := bytes.NewReader(data)
			w := &taker{header: http.Header{}}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				src.Reset(data)
				if err := paced.Send(w, src, time.Now); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkConnection is the pace's cost on a real loopback connection, 64
// MiB a response: a Writer fed gitproc.CopyBuffer's pieces — the shape a git
// response takes, chunked — beside the same writes made plainly, with no
// deadline and no flush, which is the floor the Writer is judged against; and
// a stored file sent through Send with its length declared, which reads each
// piece from its source before it writes. What the Writer adds to a write is a
// deadline, two clock readings and a flush.
func BenchmarkConnection(b *testing.B) {
	const total = 64 << 20
	piece := bytes.Repeat([]byte("crewlet!"), gitproc.CopyBuffer/8)
	file := bytes.Repeat([]byte("crewlet!"), total/8)
	arms := []struct {
		name  string
		serve func(http.ResponseWriter)
	}{
		{"writes=plain", func(rw http.ResponseWriter) {
			for range total / len(piece) {
				if _, err := rw.Write(piece); err != nil {
					return
				}
			}
		}},
		{"writes=paced", func(rw http.ResponseWriter) {
			w := paced.NewWriter(rw, time.Now)
			defer w.End()
			for range total / len(piece) {
				if _, err := w.Write(piece); err != nil {
					return
				}
			}
		}},
		{"send", func(rw http.ResponseWriter) {
			rw.Header().Set("Content-Length", strconv.Itoa(total))
			_ = paced.Send(rw, bytes.NewReader(file), time.Now)
		}},
	}
	for _, arm := range arms {
		b.Run(arm.name, func(b *testing.B) {
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				arm.serve(rw)
			}))
			defer srv.Close()
			client := srv.Client()
			b.SetBytes(total)
			for b.Loop() {
				resp, err := client.Get(srv.URL)
				if err != nil {
					b.Fatal(err)
				}
				n, err := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if err != nil || n != total {
					b.Fatalf("read %d of %d bytes: %v", n, total, err)
				}
			}
		})
	}
}
