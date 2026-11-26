package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// termHolders names, by each term of [scrubTerms], the test that holds it to
// its own figure: each such test registers itself beside it ([holdsScrubTerm]).
var termHolders = map[string]string{}

// holdsScrubTerm registers test as the one holding term.
func holdsScrubTerm(term, test string) bool {
	termHolders[term] = test
	return true
}

// termFigure is the figure of the term scrubHeldBytes adds up by name.
func termFigure(t testing.TB, name string, plain int64, longest int) int64 {
	t.Helper()
	for _, term := range scrubTerms {
		if term.name == name {
			return term.figure(plain, longest)
		}
	}
	t.Fatalf("scrubHeldBytes adds up no term %q", name)
	return 0
}

// EVERY TERM THE SCRUB ADDS UP IS HELD BY A TEST OF ITS OWN, AND EVERY TERM A
// TEST HOLDS IS ADDED UP: the table's names and the tests' registrations are
// the same set, so a term dropped from the sum, or added without its test, or
// a test's registration dropped, goes red here. Each term's value is held by
// its test, and the whole by TestTheScrubHoldsNoMoreThanItsStatedFigure.
//
// Mutation: drop a term from scrubTerms, or a test's holdsScrubTerm.
func TestEveryScrubTermIsHeldByATestOfItsOwn(t *testing.T) {
	t.Parallel()
	table := map[string]bool{}
	for _, term := range scrubTerms {
		if table[term.name] {
			t.Errorf("scrubHeldBytes adds up %q twice", term.name)
		}
		table[term.name] = true
		if termHolders[term.name] == "" {
			t.Errorf("scrubHeldBytes adds up %q, which no test holds to its figure", term.name)
		}
	}
	for name, test := range termHolders {
		if !table[name] {
			t.Errorf("%s holds %q, which scrubHeldBytes does not add up", test, name)
		}
	}
}

var _ = holdsScrubTerm("values", "TestAValueStreamHoldsAtMostFourTimesTheLongestValue")

// A VALUE STREAM HOLDS AT MOST FOUR TIMES THE LONGEST VALUE: fewer bytes than
// it from one piece to the next, and beside them a copy of those joined to
// twice it read of the next piece, however the text is cut. The "values" term
// counts that for each of a reading's two texts.
//
// Mutation: count the term at three times the longest value, the joined copy
// left out, and the stream holds past it.
func TestAValueStreamHoldsAtMostFourTimesTheLongestValue(t *testing.T) {
	t.Parallel()
	value := randomBase64(1172, 4<<10)
	s := newScrubber([]string{longValue, value})
	figure := termFigure(t, "values", 0, len(value)) / 2
	text := strings.Repeat("x "+value[:len(value)-1]+" "+value+" "+longValue+" ", 64)
	r := rand.New(rand.NewPCG(1172, 1))
	v := s.stream(io.Discard)
	most := 0
	for at := 0; at < len(text); {
		n := 1 + r.IntN(3*len(value))
		if _, err := v.Write([]byte(text[at:min(len(text), at+n)])); err != nil {
			t.Fatal(err)
		}
		at += n
		most = max(most, len(v.held)+len(v.joined))
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if int64(most) > figure {
		t.Errorf("a value stream held %d bytes, past the %d its term counts for a longest value of %d", most,
			figure, len(value))
	}
}

var _ = holdsScrubTerm("encoders", "TestAStringsEncoderHoldsARunesBytesAndWhatTheCutReads")

// A STRING'S ENCODER HOLDS A RUNE'S BYTES AND WHAT THE CUT READS: the bytes of
// a rune a piece ended inside, and at most the longest name a definition's
// cut captures, however long the string. The "encoders" term counts that for
// each of a reading's two texts.
//
// Mutation: count the term without the capture, and the encoder holds past it.
func TestAStringsEncoderHoldsARunesBytesAndWhatTheCutReads(t *testing.T) {
	t.Parallel()
	figure := termFigure(t, "encoders", 0, 0) / 2
	var out rewriteOut
	out.reset(bufio.NewWriter(io.Discard), false)
	var e jsonStringWriter
	if err := e.begin(&out, providerToolNameMax+1); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("name-é-€-𝄞-", 64)
	most := 0
	for at := 0; at < len(text); at += 5 {
		if _, err := e.Write([]byte(text[at:min(len(text), at+5)])); err != nil {
			t.Fatal(err)
		}
		most = max(most, int(unsafe.Sizeof(e.held))+len(e.capture))
	}
	if int64(most) > figure {
		t.Errorf("a string's encoder held %d bytes, past the %d its term counts", most, figure)
	}
}

var _ = holdsScrubTerm("pieces", "TestAPieceHoldsItsChunkAndItsDigests")

// A PIECE HOLDS ITS CHUNK AND ITS DIGESTS: the first chunk of its text, at
// [scrubPieceBaseBytes], and the SHA-256 states of the text as read and as
// kept. The "pieces" term counts that for each of a reading's two texts.
//
// Mutation: count the term at the chunk alone, and a piece holds past it.
func TestAPieceHoldsItsChunkAndItsDigests(t *testing.T) {
	t.Parallel()
	figure := termFigure(t, "pieces", 0, 0) / 2
	pieces := newFirstPieces(nil)
	way := &pieces.ways[textWay]
	_ = way.open(nil)
	state := int(reflect.TypeOf(sha256.New()).Elem().Size())
	digests := int(unsafe.Sizeof(pieceDigests{})) + 2*state
	if held := int64(cap(way.buf) + 2*digests); held > figure {
		t.Errorf("a piece holds %d bytes, its chunk's %d and two digests of %d, past the %d its term counts", held,
			cap(way.buf), digests, figure)
	}
}

var _ = holdsScrubTerm("rewrites", "TestAJSONRewriteHoldsItsScannerItsBufferAndAFrameADepth")

// A JSON REWRITE HOLDS ITS SCANNER, ITS BUFFER AND A FRAME A DEPTH: over a value
// nested to jsonscan.MaxDepth, the scanner's allocations as it reads it, the
// buffer a text's pieces are read into and the frames of the containers open.
// The "rewrites" term counts that for each of a reading's two rewrites. Not
// in parallel: the scanner is read off the bytes the process allocates, the
// fewest of three reads ([allocationsOf]).
//
// Mutation: count the term without its frames, and the rewrite holds past it.
func TestAJSONRewriteHoldsItsScannerItsBufferAndAFrameADepth(t *testing.T) {
	figure := termFigure(t, "rewrites", 0, 0) / 2
	depth := jsonscan.MaxDepth/2 - 1
	doc := strings.Repeat(`{"a":[`, depth) + `1` + strings.Repeat(`]}`, depth)
	_, scanned := allocationsOf(t, func() {
		sc := jsonscan.New(strings.NewReader(doc))
		for {
			if _, err := sc.Next(); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
				break
			}
		}
	})
	scanner := int64(scanned)
	j := newJSONRewrite(newScrubber([]string{longValue}))
	var out rewriteOut
	out.reset(bufio.NewWriter(io.Discard), false)
	j.reset(jsonscan.New(strings.NewReader(doc)), &out, nil)
	if err := j.run(); err != nil {
		t.Fatal(err)
	}
	frames := int64(cap(j.frames)) * int64(unsafe.Sizeof(rewriteFrame{}))
	if held := scanner + int64(cap(j.buf)) + frames; held > figure {
		t.Errorf("a JSON rewrite held %d bytes (a scanner of %d, a buffer of %d, frames of %d), past the %d "+
			"its term counts", held, scanner, cap(j.buf), frames, figure)
	}
}

var _ = holdsScrubTerm("archive", "TestAnArchivesReadersAndWritersHoldItsTerm")

// AN ARCHIVE'S READERS AND WRITERS HOLD ITS TERM: a gzipped tar read member by
// member as the first reading reads the box's, and one written through the
// kept archive's compressor, allocate together no more than the "archive"
// term, so they hold no more. Not in parallel: it is read off the bytes the
// process allocates, the fewest of three rounds ([allocationsOf]).
//
// Mutation: count the term at half, and the compressor alone passes it.
func TestAnArchivesReadersAndWritersHoldItsTerm(t *testing.T) {
	figure := termFigure(t, "archive", 0, 0)
	body := []byte(randomBase64(1173, 4<<20))
	archive := tarOf(t, map[string]string{"projects/p/s.jsonl": string(body)})
	_, allocated := allocationsOf(t, func() {
		if err := eachMember(bytes.NewReader(archive), testBounds, func(_ *tar.Header, r io.Reader) error {
			_, err := io.Copy(io.Discard, r)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		zw, err := newKeptGzip(io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		tw := tar.NewWriter(zw)
		if err = tw.WriteHeader(&tar.Header{Name: "s.jsonl", Mode: 0o600, Size: int64(len(body)),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(body); err != nil {
			t.Fatal(err)
		}
		if err = tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err = zw.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if held := int64(allocated); held > figure {
		t.Errorf("an archive's readers and writers allocated %d bytes, past the %d its term counts", held, figure)
	}
}

var _ = holdsScrubTerm("spool", "TestTheSpoolHoldsItsBufferAndAUnitsTwoWays")

// THE SPOOL HOLDS ITS BUFFER AND A UNIT'S TWO WAYS: the buffer before its gzip
// and the room each way of a unit is held in until the unit's way is known
// ([candidate]), which the "spool" term counts.
//
// Mutation: count the term without the spool's buffer, and the spool holds
// past it.
func TestTheSpoolHoldsItsBufferAndAUnitsTwoWays(t *testing.T) {
	t.Parallel()
	figure := termFigure(t, "spool", 0, 0)
	spool, err := newUnitSpool(spoolPlace{dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.remove()
	plan := &tarPlan{spool: spool}
	plan.candidate(textWay)
	plan.candidate(jsonWay)
	held := int64(spool.bw.Size() + cap(plan.candidates[textWay]) + cap(plan.candidates[jsonWay]))
	if held > figure {
		t.Errorf("the spool held %d bytes, past the %d its term counts", held, figure)
	}
}
