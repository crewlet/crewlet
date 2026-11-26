//go:build speechmodel

package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/nativelib"
)

// The known answer: the real library and the pinned model, fetched as a node
// fetches them, transcribe the fixture (testdata/README.md). It is what
// catches a C struct that drifted from the library's ABI, a pin that names the
// wrong file, and a punctuation step that stopped casing.
//
// Run by the speech workflow with -tags speechmodel (`make test-speech`). The
// library and the model are read from CREWLET_SPEECH_DIR when set (a directory
// `crewlet speech prepare` wrote), else fetched into CREWLET_SPEECH_CACHE (as
// both the native-library cache and the data directory) or a temporary
// directory, so a cache miss exercises every download.
func TestTheRealRecognizerTranscribesTheFixture(t *testing.T) {
	cache := os.Getenv("CREWLET_SPEECH_CACHE")
	if cache == "" {
		cache = t.TempDir()
	}
	t.Setenv(nativelib.CacheEnv, cache)
	logs := &bytes.Buffer{}
	logging.Configure(slog.LevelDebug, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	l := NewLocal(LocalConfig{Dir: os.Getenv("CREWLET_SPEECH_DIR"), DataDir: cache, Capacity: 2})
	defer l.Close()

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	var prepared []float64
	s, err := l.Open(ctx, Options{Lang: Lang, Preparing: func(p float64) { prepared = append(prepared, p) }})
	if err != nil {
		t.Fatalf("the real recognizer would not open: %v", err)
	}
	if len(prepared) == 0 {
		t.Error("a cold recognizer was prepared without a single progress report")
	}
	pcm := readWAV(t, filepath.Join("testdata", "librispeech-0.wav"))
	go func() {
		for i := 0; i < len(pcm); i += BytesPerSecond / 10 {
			if err := s.Send(pcm[i:min(i+BytesPerSecond/10, len(pcm))]); err != nil {
				t.Errorf("send: %v", err)
				return
			}
		}
		if err := s.Finish(); err != nil {
			t.Errorf("finish: %v", err)
		}
	}()
	var partials int
	var finals []string
	for r := range s.Results() {
		if r.Final {
			finals = append(finals, r.Text)
		} else if len(finals) == 0 {
			partials++
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("the stream ended with %v", err)
	}
	got := strings.Join(finals, " ")
	t.Logf("finals: %q after %d partials", got, partials)
	const want = "after early nightfall the yellow lamps would light up here and there the squalid quarter of the brothels"
	if words(got) != want {
		t.Errorf("transcribed %q, want the words %q", got, want)
	}
	if partials == 0 {
		t.Error("no partial arrived before the final: a person would watch an empty field")
	}
	if got == "" || strings.ToUpper(got[:1]) != got[:1] {
		t.Errorf("the final %q is not cased: the punctuation model did not run", got)
	}

	// NOTHING THE PERSON SAID REACHES A LOG (the package doc's promise).
	for _, word := range []string{"nightfall", "squalid", "brothels"} {
		if strings.Contains(strings.ToLower(logs.String()), word) {
			t.Errorf("a log line carries %q from the transcript:\n%s", word, logs.String())
		}
	}

	// A SECOND STREAM RIDES THE LOADED WORKER, with nothing to prepare.
	again, err := l.Open(ctx, Options{Lang: Lang, Preparing: func(float64) {
		t.Error("a ready recognizer reported preparation")
	}})
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

// words is text lowercased with its punctuation dropped.
func words(text string) string {
	text = strings.Map(func(r rune) rune {
		if strings.ContainsRune(".,?!;:", r) {
			return -1
		}
		return r
	}, strings.ToLower(text))
	return strings.Join(strings.Fields(text), " ")
}

// readWAV is a 16 kHz mono 16-bit WAV's PCM.
func readWAV(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Fatalf("%s is not a WAV file", name)
	}
	for i := 12; i+8 <= len(raw); {
		id, size := string(raw[i:i+4]), int(binary.LittleEndian.Uint32(raw[i+4:]))
		if id == "fmt " && (binary.LittleEndian.Uint16(raw[i+10:]) != 1 ||
			binary.LittleEndian.Uint32(raw[i+12:]) != Rate || binary.LittleEndian.Uint16(raw[i+22:]) != 16) {
			t.Fatalf("%s is not 16 kHz mono 16-bit PCM", name)
		}
		if id == "data" {
			return raw[i+8 : min(i+8+size, len(raw))]
		}
		i += 8 + size + size%2
	}
	t.Fatal(errors.New("no data chunk"))
	return nil
}
