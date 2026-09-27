package jsoncarry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
)

// Bag is a free-form JSON object whose numbers survive this build: every
// number in it decodes as a [json.Number], at every depth, and a json.Number
// encodes as the digits it was read from.
//
// A MAP OF `any` RATHER THAN OF RAW JSON because a bag is built as a literal
// by its writer and read by asserting on its values, and both stay as they
// are for a map of `any`: a reader of a number asserts json.Number, and a
// string, a bool, a list or an object decodes as encoding/json decodes it into
// `any`. Raw values would make every writer encode what it puts in and every
// reader decode what it takes out, to keep exact what the decoder keeps exact
// itself.
//
// A BUILD THAT READS A BAG AS A PLAIN map[string]any decodes each number as a
// float64, and nothing a writer does changes that reader: an integer past 2^53
// reaches it rounded. A number past what a float64 holds at all fails that
// reader's decode of the whole value holding the bag rather than rounding, so
// [Bag.MarshalJSON] refuses to write one.
type Bag map[string]any

// MarshalJSON writes the bag as encoding/json writes the map, and refuses one
// holding a number past what a float64 holds.
//
// REFUSED RATHER THAN WRITTEN, because a build that reads the bag's numbers as
// float64s fails its decode of the whole value on one it cannot hold: the
// value would be written and then lost on every such reader, where a refusal
// here is an error its writer reads. Checked on the encoded bytes, so a number
// is caught at any depth and in whatever Go value held it.
func (b Bag) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(map[string]any(b))
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if n, ok := token.(json.Number); ok {
			if _, err := strconv.ParseFloat(string(n), 64); err != nil {
				// ITS LENGTH, NOT ITS DIGITS: the number is whatever
				// size its writer made it, and the error names the one
				// fact the writer acts on.
				return nil, fmt.Errorf("jsoncarry: a bag holds a %d-byte number a "+
					"float64 cannot hold — a build that reads the bag's numbers as "+
					"float64s could not decode it at all, so carry it as a string: %w",
					len(n), err)
			}
		}
	}
}

// UnmarshalJSON decodes the bag with every number kept as its digits, and
// otherwise as encoding/json decodes a map: a member the bytes carry replaces
// what the bag held for it, one they do not carry is left as it was, and
// `null` leaves no map at all.
//
// INTO A COPY of the map the bag holds, for the reason [Unmarshal] copies a
// carry: a bag sharing its map with another value — a struct copied before
// the decode — must not change the other's.
func (b *Bag) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	bag := maps.Clone(map[string]any(*b))
	if err := decoder.Decode(&bag); err != nil {
		return err
	}
	// ONE VALUE, as json.Unmarshal takes: a decoder stops after the first
	// and would accept whatever follows it.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("jsoncarry: a bag's bytes hold more than one JSON value")
	}
	*b = bag
	return nil
}
