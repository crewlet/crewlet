package executor

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"testing"
)

// A SPARSE MEMBER, BUILT BY HAND: no pack makes one (a box's tar archives the
// files a harness wrote, and sparsely only when asked to), and archive/tar's
// writer drops the records that make one. It is what a box's own agent could
// leave in place of what its run packed: a few blocks of archive that hand
// their reader the holes they declare, which the archive does not hold. A
// person's transcript and a relaunch's reading each meter what a member hands
// out apart from what the archive inflates to, against exactly this.

// tarBlock is one header block of a ustar archive naming name, of typeflag,
// with size bytes of data after it.
func tarBlock(name string, typeflag byte, size int) []byte {
	var b [512]byte
	copy(b[0:100], name)
	copy(b[100:108], "0000644\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	copy(b[124:136], fmt.Sprintf("%011o\x00", size))
	copy(b[136:148], fmt.Sprintf("%011o\x00", 0))
	b[156] = typeflag
	copy(b[257:263], "ustar\x00")
	copy(b[263:265], "00")
	// THE CHECKSUM IS OF THE BLOCK with its own field read as spaces.
	copy(b[148:156], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return b[:]
}

// paxRecord is one record of a PAX extended header: its whole length in
// decimal, the length's own digits included, then `key=value` and a newline.
func paxRecord(key, value string) string {
	body := " " + key + "=" + value + "\n"
	n := len(body) + 1
	for len(strconv.Itoa(n))+len(body) != n {
		n = len(strconv.Itoa(n)) + len(body)
	}
	return strconv.Itoa(n) + body
}

// padded is data and the zeros that fill out its last block.
func padded(data []byte) []byte {
	if rem := len(data) % 512; rem != 0 {
		data = append(data, make([]byte, 512-rem)...)
	}
	return data
}

// sparseSegment is one stretch of a sparse member that holds data: data, at
// offset into the member.
type sparseSegment struct {
	offset int64
	data   string
}

// sparseMember is the blocks of one tar member named name, a GNU sparse file
// (PAX format 1.0) declaring size bytes: each of segments, in order, holding
// its data, and every byte between them a hole, read as a NUL.
//
// THE MEMBER'S DATA IS ITS SPARSE MAP, then the segments' data: the map says
// how many segments there are and, for each, its offset and its length, and is
// filled out to a block.
func sparseMember(name string, size int64, segments ...sparseSegment) []byte {
	pax := paxRecord("GNU.sparse.major", "1") + paxRecord("GNU.sparse.minor", "0") +
		paxRecord("GNU.sparse.name", name) + paxRecord("GNU.sparse.realsize", strconv.FormatInt(size, 10))
	var sparseMap, data strings.Builder
	sparseMap.WriteString(strconv.Itoa(len(segments)) + "\n")
	for _, s := range segments {
		sparseMap.WriteString(strconv.FormatInt(s.offset, 10) + "\n" + strconv.Itoa(len(s.data)) + "\n")
		data.WriteString(s.data)
	}
	stored := append(padded([]byte(sparseMap.String())), data.String()...)
	var raw bytes.Buffer
	raw.Write(tarBlock("PaxHeaders/"+path.Base(name), 'x', len(pax)))
	raw.Write(padded([]byte(pax)))
	raw.Write(tarBlock("GNUSparseFile.0/"+path.Base(name), '0', len(stored)))
	raw.Write(padded(stored))
	return raw.Bytes()
}

// plainMember is the blocks of one tar member named name, a regular file
// holding body, to stand beside sparse members in one tar.
func plainMember(name, body string) []byte {
	return append(tarBlock(name, '0', len(body)), padded([]byte(body))...)
}

// sparseTar is a gzipped tar of members, each the blocks [sparseMember] gives,
// in order, ended as a tar is.
func sparseTar(t *testing.T, members ...[]byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	for _, m := range members {
		raw.Write(m)
	}
	raw.Write(make([]byte, 1024))
	return gzipOf(t, raw.String())
}

// sparseBomb is a gzipped tar whose one member is named as a session's
// transcript and is a sparse file declaring size bytes, every one of them a
// hole.
func sparseBomb(t *testing.T, size int64) []byte {
	t.Helper()
	return sparseTar(t, sparseMember("projects/"+claudeProjectDir+"/s.jsonl", size))
}

// A SPARSE MEMBER HANDS OUT ITS SEGMENTS WHERE ITS MAP PUTS THEM, AND NULS
// EVERYWHERE ELSE, up to the size it declares, out of a stream that holds its
// segments alone: what the cases that build one rely on it to be.
//
// Mutation: write a segment's data where its map does not say, or fill the map
// past a block, and the member reads otherwise or not at all.
func TestASparseMemberHandsOutWhatItsMapSays(t *testing.T) {
	t.Parallel()
	const name = "projects/" + claudeProjectDir + "/s.jsonl"
	archive := sparseTar(t, sparseMember(name, 4096, sparseSegment{0, "first\n"}, sparseSegment{4095, "\n"}))
	var got []byte
	var names []string
	if err := eachMember(bytes.NewReader(archive), boundsFor(1<<20), func(h *tar.Header, member io.Reader) error {
		names = append(names, h.Name)
		var err error
		got, err = io.ReadAll(member)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := "first\n" + strings.Repeat("\x00", 4095-len("first\n")) + "\n"
	if len(names) != 1 || names[0] != name || string(got) != want {
		t.Errorf("the archive read as %q holding %d bytes; want one member %q of %d bytes, its segments where "+
			"its map puts them", names, len(got), name, len(want))
	}
	if len(archive) > 1024 {
		t.Errorf("the archive is %d bytes gzipped; want its holes held nowhere, a few hundred bytes", len(archive))
	}
}
