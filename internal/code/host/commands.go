package host

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A PUSH'S COMMAND SECTION: the ref updates git sends before the pack, read
// so the engine can refuse what it can refuse without objects — and then handed
// to receive-pack byte for byte, followed by the rest of the body, so git
// reads exactly what the client sent.

// capabilities are the ones a push's first command names that the engine
// reads.
type capabilities struct {
	reportStatus, reportStatusV2, sideBand64k, atomic, quiet, pushOptions bool
	agent                                                                 string
}

// parseCapabilities reads a NUL-separated list's tail: space-separated words.
func parseCapabilities(list string) capabilities {
	var c capabilities
	for word := range strings.FieldsSeq(list) {
		switch {
		case word == "report-status":
			c.reportStatus = true
		case word == "report-status-v2":
			c.reportStatusV2 = true
		case word == "side-band-64k":
			c.sideBand64k = true
		case word == "atomic":
			c.atomic = true
		case word == "quiet":
			c.quiet = true
		case word == "push-options":
			c.pushOptions = true
		case strings.HasPrefix(word, "agent="):
			c.agent = strings.TrimPrefix(word, "agent=")
		}
	}
	return c
}

// parsedCommand is one ref update as the client sent it.
type parsedCommand struct{ Old, New, Ref string }

// section is a push's command section.
type section struct {
	// Raw is every byte read, the flush included — joined back in front of
	// the rest of the body for receive-pack.
	Raw []byte
	// Commands are the updates in the client's order.
	Commands []parsedCommand
	// Shallow counts the `shallow <oid>` lines (passed through).
	Shallow int
	// Caps are the first command's capabilities.
	Caps capabilities
}

// Empty reports a section with no command and no shallow line: git's probe
// before a push larger than its post buffer.
func (s section) Empty() bool { return len(s.Commands) == 0 && s.Shallow == 0 }

// The two bounds a command section is held to, each its own refusal.
var (
	errSectionTooLarge = errors.New("host: the push's command list is over a mebibyte")
	errTooManyCommands = fmt.Errorf("host: the push names more than %d refs", code.MaxCommandsPerPush)
)

// readCommands reads pkt-lines to the first flush. A line `shallow <oid>` is
// counted and passed; a line `<old> <new> <ref>[\0caps]` — two object ids and
// a ref of at most code.MaxRefNameBytes — is a command; ANY OTHER LINE (a
// push-cert the engine never advertises, garbage) is passed uncounted, for
// receive-pack to refuse in its own words: nothing here pre-checks a command it
// cannot parse. errSectionTooLarge past maxBytes read; errTooManyCommands at
// the maxCommands+1th command. A read error or a malformed packet is returned
// as itself (the body could not be read as git's).
func readCommands(r *bufio.Reader, maxBytes, maxCommands int) (section, error) {
	var s section
	for first := true; ; {
		p, err := readPkt(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return s, err
		}
		raw := encodePkt(p)
		if len(s.Raw)+len(raw) > maxBytes {
			return s, errSectionTooLarge
		}
		s.Raw = append(s.Raw, raw...)
		if p.flush {
			return s, nil
		}
		if p.delim || p.end {
			continue
		}
		line := bytes.TrimSuffix(p.payload, []byte{'\n'})
		if oid, ok := bytes.CutPrefix(line, []byte("shallow ")); ok && gitref.ValidOID(string(oid)) {
			s.Shallow++
			continue
		}
		cmd, caps, ok := parseCommand(line)
		if !ok {
			continue
		}
		if len(s.Commands) == maxCommands {
			return s, errTooManyCommands
		}
		if first {
			s.Caps, first = parseCapabilities(caps), false
		}
		s.Commands = append(s.Commands, cmd)
	}
}

// parseCommand reads `<old> <new> <ref>[\0caps]`.
func parseCommand(line []byte) (parsedCommand, string, bool) {
	body, caps, _ := bytes.Cut(line, []byte{0})
	fields := strings.SplitN(string(body), " ", 3)
	if len(fields) != 3 || !gitref.ValidOID(fields[0]) || !gitref.ValidOID(fields[1]) ||
		fields[2] == "" || len(fields[2]) > code.MaxRefNameBytes {
		return parsedCommand{}, "", false
	}
	return parsedCommand{Old: fields[0], New: fields[1], Ref: fields[2]}, string(caps), true
}
