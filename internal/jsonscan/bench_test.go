package jsonscan_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// BenchmarkALongBase64String reads a line holding one long base64 string, an image's or a PDF's as Claude
// Code writes it, 16 MiB of it: every text read through a 64 KiB p, as the scrub streams it through its
// redaction; the line passed over by Skip, as the thread layer passes media over; and, for scale, read by
// jsontext, which holds the string whole.
func BenchmarkALongBase64String(b *testing.B) {
	media := make([]byte, 12<<20)
	rand.NewChaCha8([32]byte{1}).Read(media)
	line := `{"type":"image","source":{"type":"base64","media_type":"application/pdf","data":"` +
		base64.StdEncoding.EncodeToString(media) + `"}}`
	benchmarkLine(b, []byte(line))
}

// BenchmarkATranscriptLikeLine reads a Claude Code record line whose tool result holds about 1 MiB of
// source code and prose, escapes and characters past ASCII among them, twice over (the result's content and
// its toolUseResult), under the record's head: the shape of a session's long lines that are not media.
func BenchmarkATranscriptLikeLine(b *testing.B) {
	var text strings.Builder
	for text.Len() < 1<<20 {
		text.WriteString("func (s *Scanner) Next() (Kind, error) {\n\tif s.err != nil {\n\t\treturn \"\", s.err\n\t}\n")
		text.WriteString("// café, naïve résumé, 日本語, \U0001F600, a quote \" and a " +
			"backslash \\ and <tags> & ampersands, then a long plain sentence that runs on as prose does\n")
	}
	var quoted bytes.Buffer
	enc := json.NewEncoder(&quoted)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(text.String()); err != nil {
		b.Fatal(err)
	}
	inner := strings.TrimSuffix(quoted.String(), "\n")
	line := `{"parentUuid":"0b0f6c62-2a5c-4d8e-9a43-5f3f1d0e9c21","isSidechain":false,"userType":"external",` +
		`"cwd":"/work","sessionId":"9f1c2b7e-0c1d-4e8f-a9b2-3c4d5e6f7a8b","version":"2.1.322","type":"user",` +
		`"message":{"role":"user","content":[{"tool_use_id":"toolu_01ABCDEF","type":"tool_result","content":[` +
		`{"type":"text","text":` + inner + `}]}]},"uuid":"5f0c9e1a-7b2d-4c3e-8f9a-0b1c2d3e4f5a",` +
		`"timestamp":"2026-11-14T10:00:00.000Z","toolUseResult":{"stdout":` + inner +
		`,"stderr":"","interrupted":false,"isImage":false}}`
	benchmarkLine(b, []byte(line))
}

// BenchmarkALongTextPastASCII reads a line holding 16 MiB of text past ASCII, CJK and emoji, three and four
// bytes a character, among plain words: the shape a counted text costs most on, every character decoded
// for its units ([jsonscan.Scanner.CountUnits]).
func BenchmarkALongTextPastASCII(b *testing.B) {
	var text strings.Builder
	for text.Len() < 16<<20 {
		text.WriteString("日本語のテキスト 😀🚀 한국어 текст, words between them ")
	}
	benchmarkLine(b, []byte(`{"type":"text","text":"`+text.String()+`"}`))
}

// benchmarkLine reads line four ways, each a reading of the whole line per iteration: read, passed over by
// Skip, passed over by Skip counting its units, and by jsontext.
func benchmarkLine(b *testing.B, line []byte) {
	b.Run("read", func(b *testing.B) {
		b.SetBytes(int64(len(line)))
		b.ReportAllocs()
		r := bytes.NewReader(line)
		s := jsonscan.New(r)
		p := make([]byte, 64<<10)
		for b.Loop() {
			r.Reset(line)
			s.Reset(r)
			for {
				k, err := s.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					b.Fatal(err)
				}
				if !hasText(k) {
					continue
				}
				for {
					if _, err = s.Read(p); errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		}
	})
	b.Run("skip", func(b *testing.B) {
		b.SetBytes(int64(len(line)))
		b.ReportAllocs()
		r := bytes.NewReader(line)
		s := jsonscan.New(r)
		for b.Loop() {
			r.Reset(line)
			s.Reset(r)
			if _, err := s.Next(); err != nil {
				b.Fatal(err)
			}
			if err := s.Skip(); err != nil {
				b.Fatal(err)
			}
			if _, err := s.Next(); !errors.Is(err, io.EOF) {
				b.Fatal(err)
			}
		}
	})
	b.Run("skip_counting", func(b *testing.B) {
		b.SetBytes(int64(len(line)))
		b.ReportAllocs()
		r := bytes.NewReader(line)
		s := jsonscan.New(r)
		for b.Loop() {
			r.Reset(line)
			s.Reset(r)
			s.CountUnits(true)
			if _, err := s.Next(); err != nil {
				b.Fatal(err)
			}
			if err := s.Skip(); err != nil {
				b.Fatal(err)
			}
			if _, err := s.Next(); !errors.Is(err, io.EOF) || s.Units() < int64(len(line))/4 {
				b.Fatal(err, s.Units())
			}
		}
	})
	b.Run("jsontext", func(b *testing.B) {
		b.SetBytes(int64(len(line)))
		b.ReportAllocs()
		r := bytes.NewReader(line)
		dec := jsontext.NewDecoder(r, threadDecoding)
		for b.Loop() {
			r.Reset(line)
			dec.Reset(r, threadDecoding)
			for {
				tok, err := dec.ReadToken()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					b.Fatal(err)
				}
				if tok.Kind() == '"' {
					_ = tok.String()
				}
			}
		}
	})
}
