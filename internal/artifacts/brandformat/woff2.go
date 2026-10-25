package brandformat

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ---- WOFF2 ---------------------------------------------------------------- //
//
// A FACE IS WOFF2 ALONE, CHECKED BY ITS HEADER. Its tables are Brotli
// streams, and decoding them would bring a decompressor into the engine to
// learn what the browser's own font sanitiser (OpenType Sanitizer in Chrome
// and Firefox) checks again anyway; what the engine holds is what bounds the
// work a page's reader is put to: the signature, a header length equal to the
// file, a table count from 1 to [MaxFontTables], the size the font decodes to
// at most [MaxFontSFNTBytes], a compressed size inside the file, a metadata
// block (if any) inside the file, and no private data block: a font's private
// block draws nothing and would be served as uploaded, for ever. Who may
// push a face into every reader's browser is bounded by the adoption, which
// is an administrator's.

const woff2HeaderBytes = 48

// The flavours a WOFF2 font may wrap: TrueType outlines and CFF ones.
const (
	flavorTrueType = 0x00010000
	flavorCFF      = 0x4f54544f // "OTTO"
)

// checkWOFF2 holds a WOFF2 file's header to the bounds.
func checkWOFF2(data []byte) error {
	if msg := TooLarge(fontEssence, int64(len(data))); msg != "" {
		return errors.New(msg)
	}
	if len(data) < woff2HeaderBytes || string(data[0:4]) != "wOF2" {
		return errors.New("the file is not a WOFF2 font: convert the face to WOFF2")
	}
	flavor := binary.BigEndian.Uint32(data[4:8])
	length := binary.BigEndian.Uint32(data[8:12])
	tables := binary.BigEndian.Uint16(data[12:14])
	reserved := binary.BigEndian.Uint16(data[14:16])
	sfnt := binary.BigEndian.Uint32(data[16:20])
	compressed := binary.BigEndian.Uint32(data[20:24])
	metaOffset, metaLength := binary.BigEndian.Uint32(data[28:32]), binary.BigEndian.Uint32(data[32:36])
	privOffset, privLength := binary.BigEndian.Uint32(data[40:44]), binary.BigEndian.Uint32(data[44:48])
	switch {
	case flavor != flavorTrueType && flavor != flavorCFF:
		return fmt.Errorf("the font's flavour %#08x is neither TrueType nor CFF", flavor)
	case uint64(length) != uint64(len(data)):
		return fmt.Errorf("the font's header says %d bytes and the file holds %d: the file is damaged",
			length, len(data))
	case tables < 1 || tables > MaxFontTables:
		return fmt.Errorf("the font holds %d tables and a face holds 1 to %d", tables, MaxFontTables)
	case reserved != 0:
		return errors.New("the font's header sets its reserved field: the file is damaged")
	case sfnt > MaxFontSFNTBytes:
		return fmt.Errorf("the font decodes to %d bytes and a face may decode to at most %d: subset it",
			sfnt, MaxFontSFNTBytes)
	case uint64(compressed) > uint64(len(data)-woff2HeaderBytes):
		return errors.New("the font's compressed tables are longer than the file: the file is damaged")
	case metaLength == 0 && metaOffset != 0,
		metaLength > 0 && (metaOffset < woff2HeaderBytes ||
			uint64(metaOffset)+uint64(metaLength) > uint64(len(data))):
		return errors.New("the font's metadata block lies outside the file: the file is damaged")
	case privLength > 0 || privOffset != 0:
		return errors.New("the font carries a private data block, which draws nothing and would " +
			"be served as uploaded: export the face again without one")
	}
	return nil
}
