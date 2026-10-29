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
//
// IT IS READ AS RECEIVE-PACK READS IT — read_head_info and queue_command, in
// git's builtin/receive-pack.c — because every check before receive-pack runs
// (the stateless refusals, the record's budget, a ref named twice, the
// protections) is a check of the commands this reader found, and receive-pack
// acts on the commands IT found. Read more narrowly, a command git queues was
// one no check had seen: ids written in capitals parsed as no command here, so
// a push of hundreds of the widest refs in capitals passed step 2 unmeasured,
// ran receive-pack, stored its bundle and was refused by the receiver's own
// proposal check as the receiver's bug, logged at ERROR. Every rule below is
// git's, measured against git 2.43 (TestTheCommandSectionIsReadAsReceivePackReadsIt).

// capabilities are the ones receive-pack reads off a push's lines that the
// engine reads too: the report and its framing for a refusal the receiver
// writes itself, --atomic for its refusal of the whole push, and
// push-options, after which receive-pack reads the client's options before
// the pack — so the section a push is bounded by runs to their end
// (readCommands). The rest receive-pack reads for itself.
type capabilities struct {
	reportStatus, reportStatusV2, sideBand64k, atomic, quiet, pushOptions bool
}

// add reads one line's capability list into c as read_head_info does: EVERY
// line but a shallow one may carry a list after its NUL — git sends one on its
// first command, and receive-pack reads one from whichever line carries it —
// and a list asks only for what it names; nothing a later line says unasks it.
// The list is a C string to receive-pack, so it ends at the next NUL. atomic
// is read because receive-pack honours it whenever it advertises it, which it
// does unless receive.advertiseAtomic is false, and the engine never sets it;
// push-options likewise, which it honours whenever
// receive.advertisePushOptions is true, as the receive configuration sets it
// (receiveConfig).
func (c *capabilities) add(list []byte) {
	list, _, _ = bytes.Cut(list, []byte{0})
	c.reportStatus = c.reportStatus || asks(list, "report-status")
	c.reportStatusV2 = c.reportStatusV2 || asks(list, "report-status-v2")
	c.sideBand64k = c.sideBand64k || asks(list, "side-band-64k")
	c.atomic = c.atomic || asks(list, "atomic")
	c.quiet = c.quiet || asks(list, "quiet")
	c.pushOptions = c.pushOptions || asks(list, "push-options")
}

// asks is git's parse_feature_request, its loop and all: feature found where
// a scan of list starts or after one of git's four spaces, and followed by the
// list's end, a space or `=` (a feature with a value). A match that is part of
// a longer word moves the scan on by one byte, as git's does — so none of the
// features above, whose spellings do not overlap themselves, is ever found
// inside another word, and report-status is never found in report-status-v2.
func asks(list []byte, feature string) bool {
	for len(list) > 0 {
		i := bytes.Index(list, []byte(feature))
		if i < 0 {
			return false
		}
		if i == 0 || gitSpace(list[i-1]) {
			if rest := list[i+len(feature):]; len(rest) == 0 || gitSpace(rest[0]) || rest[0] == '=' {
				return true
			}
		}
		list = list[i+1:]
	}
	return false
}

// gitSpace is git's isspace (sane_ctype): a space, a tab, a line feed or a
// carriage return — never a vertical tab or a form feed, which a Unicode or C
// library reading of "space" takes too.
func gitSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// parsedCommand is one ref update as the client sent it, its ids in their
// lowercase spelling (wireOID).
type parsedCommand struct{ Old, New, Ref string }

// section is a push's command section.
type section struct {
	// Raw is every byte read, the packet that ended it included — and the
	// push's options after it with the packet that ended them, where the
	// client asked for push-options — joined back in front of the rest of the
	// body for receive-pack.
	Raw []byte
	// Commands are the updates in the client's order.
	Commands []parsedCommand
	// Caps are what the section's lines asked for (capabilities.add).
	Caps capabilities
}

// Empty reports a section that is a LONE FLUSH — git's probe before a push
// larger than its post buffer, a body of exactly `0000` — and nothing else.
// A section holding any line at all is a push, whatever the receiver made of
// the line: one that is neither a command nor a shallow line is a line
// receive-pack dies on (a protocol error), refused in git's own words, and the
// push is recorded. Judged by what
// was parsed, a section of lines the receiver could not read — a ref past the
// longest, once — was answered as the probe: an empty 200, the body never
// read, receive-pack never run, nothing recorded, and git printing only that
// the remote end hung up.
func (s section) Empty() bool { return string(s.Raw) == flushPkt }

// The bounds a command section is held to, and the one line it refuses, each
// its own refusal.
var (
	errSectionTooLarge = errors.New("host: the push's command list is over a mebibyte")
	// errOptionsTooLarge is a push whose options take its command section
	// past the bound: receive-pack holds every option a client sends in its
	// memory, so a push past it is refused before receive-pack reads one.
	errOptionsTooLarge = errors.New("host: the push's options take its command list past a mebibyte")
	errTooManyCommands = fmt.Errorf("host: the push names more than %d refs", code.MaxCommandsPerPush)
	// errPushCertificate is a push certificate — the start of a signed push.
	// The engine never advertises push-cert (receive.certNonceSeed is unset),
	// so git's own client never sends one, and `git push --signed` stops in
	// the client. But receive-pack reads one WHATEVER IT ADVERTISED and queues
	// the commands the certificate's body names, which no pkt-line of their
	// own carries — so no reader of lines could have checked them, and the
	// push is refused before anything runs.
	errPushCertificate = errors.New("host: the push carries a push certificate (a signed push)")
)

// readCommands reads a push's command section as read_head_info does, each
// pkt-line's one trailing line feed dropped as git's packet reader drops it:
//
//   - the section ends at the first packet that is no line — a flush, as git
//     sends, or a delimiter or a response end, at which receive-pack stops
//     reading commands just as it does at a flush, and reads what follows as
//     the pack;
//   - a line `shallow ` and more is a shallow line, never a command, and its
//     capabilities are not read: receive-pack takes it as one (an id at
//     bytes 8–48, whatever follows) or dies on it, and acts on neither;
//   - every other line's capabilities, after its NUL, are read (add);
//   - a line `push-cert` is refused, errPushCertificate;
//   - a line `<old> <new> <ref>` is a command, read by parseCommand;
//   - ANY OTHER LINE — an `ERR` packet, a line of fewer bytes than two ids,
//     an id that is not hex — is one receive-pack dies on as a protocol error
//     before it acts on any command, and is passed, uncounted, for it to
//     refuse in its own words.
//
// AND THE PUSH'S OPTIONS AFTER IT, where the section queued a command and a
// line asked for push-options: the pkt-lines receive-pack reads next, to the
// first packet that is no line, before the pack (readPushOptions).
//
// errSectionTooLarge past maxBytes read, errOptionsTooLarge past it with the
// options; errTooManyCommands at the maxCommands+1th command. A read error or
// a malformed packet is returned as itself (the body could not be read as
// git's).
func readCommands(r *bufio.Reader, maxBytes, maxCommands int) (section, error) {
	var s section
	for {
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
		if p.flush || p.delim || p.end {
			// receive-pack reads options only of a push whose section
			// queued a command: one that queued none it acts on nothing of.
			if len(s.Commands) == 0 || !s.Caps.pushOptions {
				return s, nil
			}
			return s, readPushOptions(r, &s, maxBytes)
		}
		line := bytes.TrimSuffix(p.payload, []byte{'\n'})
		if len(line) > len("shallow ") && bytes.HasPrefix(line, []byte("shallow ")) {
			continue
		}
		body, caps, hasCaps := bytes.Cut(line, []byte{0})
		if hasCaps {
			s.Caps.add(caps)
		}
		if string(body) == "push-cert" {
			return s, errPushCertificate
		}
		cmd, ok := parseCommand(body)
		if !ok {
			continue
		}
		if len(s.Commands) == maxCommands {
			return s, errTooManyCommands
		}
		s.Commands = append(s.Commands, cmd)
	}
}

// readPushOptions reads a push's options into s.Raw — the pkt-lines
// receive-pack reads after the commands of a client that asked for
// push-options (read_push_options), to the first packet that is no line, that
// packet included — held to what maxBytes leaves of the section: receive-pack
// keeps every option a client sends in its memory, with no bound of git's, so
// a push whose options take the section past it is refused before receive-pack
// reads one (errOptionsTooLarge). The bound is the commands' own because a
// change's options at their longest are some 33 KiB, beside the 340 KiB the
// most commands the engine takes come to, and every other option is a forge's
// that a push to the code host has no reason to send.
//
// WHAT THEY SAY IS READ BY NOBODY HERE. The relay reads the options
// receive-pack hands the push's hook, which are what it read and accepted
// (session.go); bytes the client wrote beside them are no option of the push.
func readPushOptions(r *bufio.Reader, s *section, maxBytes int) error {
	for {
		p, err := readPkt(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		raw := encodePkt(p)
		if len(s.Raw)+len(raw) > maxBytes {
			return errOptionsTooLarge
		}
		s.Raw = append(s.Raw, raw...)
		if p.flush || p.delim || p.end {
			return nil
		}
	}
}

// commandsRefusal is the sentence a push is refused in, before its work, when
// its commands are more than one push may carry
// (code.Proposal.ValidateCommands) — "" when they are not. The commands are
// the section's as they will be proposed: each ref in its recorded form
// (code.RecordedRef), and the refusals pre has already made — under the
// principal the push is admitted as and the way the relay's landing names
// (code.ViaGit), both of which its entry carries.
//
// The push's width is said in sizes a person reads, with what to do; any other
// rule — a ref named twice, a command that moves nothing, which only a
// hand-built client sends — in the domain's own words.
func commandsRefusal(adm admission, node string, sec section, pre map[string]answered) string {
	if len(sec.Commands) == 0 {
		// A SECTION OF LINES THAT PARSE AS NO COMMAND is one receive-pack
		// acts on no command of (readCommands): it dies on a line that is
		// none, in its own words, and nothing is proposed from it.
		return ""
	}
	cmds := make([]code.Command, len(sec.Commands))
	for i, c := range sec.Commands {
		cmds[i] = code.Command{Ref: code.RecordedRef(c.Ref), Old: c.Old, New: c.New}
		if a, ok := pre[c.Ref]; ok {
			cmds[i].Result, cmds[i].Reason = code.ResultNG, a.Reason
		}
	}
	err := code.Proposal{Principal: adm.principal(), Via: code.ViaGit, Node: node,
		Commands: cmds}.ValidateCommands()
	var wide *code.TooWideError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &wide):
		return fmt.Sprintf("this push's ref updates take up to %s of its record, past the %s one push may; "+
			"push them in parts (git push origin <some refs>)", humanBytes(int64(wide.Bytes)),
			humanBytes(int64(wide.Room)))
	}
	return "this push's command list is not one the engine takes: " +
		strings.TrimPrefix(err.Error(), code.ErrInvalidProposal.Error()+": ")
}

// recordedCommands is cmds as a proposal carries them — each ref in its
// recorded form (code.RecordedRef) — and the ref git named each recorded form
// for, to answer git with ([withRefs]).
func recordedCommands(cmds []code.Command) ([]code.Command, map[string]string) {
	out := make([]code.Command, len(cmds))
	named := make(map[string]string, len(cmds))
	for i, c := range cmds {
		c.Ref = code.RecordedRef(c.Ref)
		named[c.Ref] = cmds[i].Ref
		out[i] = c
	}
	return out, named
}

// withRefs is cmds with each recorded ref replaced by the ref git named:
// git matches a status to its command by the ref's name, so an answer read
// from a row, which holds a ref the engine refused in its bounded form, is
// told to git with the ref the client sent.
func withRefs(cmds []code.Command, named map[string]string) []code.Command {
	out := make([]code.Command, len(cmds))
	for i, c := range cmds {
		if ref, ok := named[c.Ref]; ok {
			c.Ref = ref
		}
		out[i] = c
	}
	return out
}

// parseCommand reads a line's `<old> <new> <ref>` — the line to its NUL — as
// queue_command does: an object id of hex digits in EITHER case (wireOID, which
// spells it lowercase, as git spells it to the hook), a space, another, a space,
// and the ref THE REST OF THE LINE, whatever it holds — a space, a carriage
// return, nothing at all. Every ref is a command here, at ANY LENGTH the
// pkt-line carries, because whether the engine takes it is a REFUSAL — bad_ref
// (code.StatelessRefusal), which the receiver answers in the report git reads
// and records — never a condition of being a command: held to
// code.MaxRefNameBytes, a ref git accepts and the engine does not was no
// command, a push of nothing else read as git's probe, and the capabilities
// its line carried went unread. An empty ref git refuses itself (`funny
// refname`) and hands no hook, and the receiver refuses it bad_ref.
func parseCommand(line []byte) (parsedCommand, bool) {
	const n = gitref.OIDHexLen
	if len(line) < 2*n+2 || line[n] != ' ' || line[2*n+1] != ' ' {
		return parsedCommand{}, false
	}
	old, okOld := wireOID(line[:n])
	updated, okNew := wireOID(line[n+1 : 2*n+1])
	if !okOld || !okNew {
		return parsedCommand{}, false
	}
	return parsedCommand{Old: old, New: updated, Ref: string(line[2*n+2:])}, true
}
