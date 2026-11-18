package host

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A push's command section: the ref updates git sends before the pack, read so
// the engine can refuse what it can without objects, then handed to
// receive-pack byte for byte ahead of the rest of the body.
//
// It is read as receive-pack reads it (read_head_info and queue_command in
// git's builtin/receive-pack.c), because every check before receive-pack runs
// (the stateless refusals, the record's budget, a ref named twice, the
// protections) judges the commands this reader found, while receive-pack acts
// on the ones it found: read more narrowly, git queues a command no check saw.
// Every rule below is git's, measured against git 2.43
// (TestTheCommandSectionIsReadAsReceivePackReadsIt).

// capabilities are those receive-pack reads off a push's lines that the engine
// reads too: the report and its framing for a refusal the receiver writes
// itself, atomic for refusing the whole push, and push-options, after which
// receive-pack reads the options before the pack, so the section runs to their
// end (readCommands).
type capabilities struct {
	reportStatus, reportStatusV2, sideBand64k, atomic, quiet, pushOptions bool
}

// add reads one line's capability list into c as read_head_info does: any line
// but a shallow one may carry a list after its NUL, the list ends at the next
// NUL (a C string to receive-pack), and it only adds: no later line unasks a
// capability. atomic and push-options are read because receive-pack honours
// them whenever it advertises them: atomic unless receive.advertiseAtomic is
// false, which the engine never sets, and push-options since receiveConfig sets
// receive.advertisePushOptions.
func (c *capabilities) add(list []byte) {
	list, _, _ = bytes.Cut(list, []byte{0})
	c.reportStatus = c.reportStatus || asks(list, "report-status")
	c.reportStatusV2 = c.reportStatusV2 || asks(list, "report-status-v2")
	c.sideBand64k = c.sideBand64k || asks(list, "side-band-64k")
	c.atomic = c.atomic || asks(list, "atomic")
	c.quiet = c.quiet || asks(list, "quiet")
	c.pushOptions = c.pushOptions || asks(list, "push-options")
}

// asks is git's parse_feature_request, loop and all: feature found at the
// scan's start or after a gitSpace, followed by the list's end, a space or `=`.
// A match inside a longer word moves the scan on one byte, as git's does; the
// features above do not overlap themselves, so none is found inside another
// word (report-status never in report-status-v2).
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

// gitSpace is git's isspace (sane_ctype): space, tab, LF or CR, never the
// vertical tab or form feed a C or Unicode isspace takes too.
func gitSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// parsedCommand is one ref update as the client sent it, its ids in their
// lowercase spelling (wireOID).
type parsedCommand struct{ Old, New, Ref string }

// section is a push's command section.
type section struct {
	// Raw is every byte read, through the packet that ended it, and the
	// push's options after it through their ending packet where the client
	// asked for push-options; it goes back in front of the body for
	// receive-pack.
	Raw []byte
	// Commands are the updates in the client's order.
	Commands []parsedCommand
	// Caps are what the section's lines asked for (capabilities.add).
	Caps capabilities
}

// Empty reports a section that is a lone flush (`0000`, git's probe before a
// push larger than its post buffer). It is judged by the raw bytes, not by
// what parsed: a section holding any line is a push, and a line that is
// neither a command nor shallow is one receive-pack dies on in its own words,
// with the push recorded. Answered as the probe, it would skip receive-pack,
// record nothing and leave git saying only that the remote end hung up.
func (s section) Empty() bool { return string(s.Raw) == flushPkt }

// The bounds a command section is held to, and the one line it refuses, each
// its own refusal.
var (
	errSectionTooLarge = errors.New("host: the push's command list is over a mebibyte")
	// errOptionsTooLarge is a push whose options take its section past the
	// bound, refused before receive-pack, which holds every option in memory,
	// reads one.
	errOptionsTooLarge = errors.New("host: the push's options take its command list past a mebibyte")
	errTooManyCommands = fmt.Errorf("host: the push names more than %d refs", gitlet.MaxCommandsPerPush)
	// errPushCertificate is a push certificate (a signed push). The engine never
	// advertises push-cert (receive.certNonceSeed is unset), so git's client
	// never sends one, but receive-pack reads one whatever it advertised and
	// queues the commands its body names, which no pkt-line of their own
	// carries: no check could see them, so the push is refused before anything
	// runs.
	errPushCertificate = errors.New("host: the push carries a push certificate (a signed push)")
)

// readCommands reads a push's command section as read_head_info does, each
// pkt-line's trailing LF dropped: it ends at the first packet that is no line
// (flush, delimiter or response end, after which receive-pack reads the pack);
// a `shallow ` line is skipped, its capabilities unread; every other line's
// capabilities after its NUL are read (add); `push-cert` is refused
// (errPushCertificate); `<old> <new> <ref>` is a command (parseCommand); any
// other line is one receive-pack dies on before acting on any command, so it
// passes uncounted for receive-pack to refuse in its own words. Where a command
// was queued and push-options asked for, the options are read too
// (readPushOptions). errSectionTooLarge past maxBytes, errOptionsTooLarge past
// it with the options, errTooManyCommands at command maxCommands+1; a read or
// packet error is returned as itself.
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
			// receive-pack reads options only after a queued command.
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

// readPushOptions reads into s.Raw the pkt-lines receive-pack reads after the
// commands when push-options was asked for (read_push_options), through the
// first packet that is no line. receive-pack keeps every option in memory with
// no bound of its own, so they share the section's maxBytes
// (errOptionsTooLarge): a change's options come to some 33 KiB at most, beside
// the 340 KiB of the most commands the engine takes, and other options are a
// forge's that a push to Gitlet has no reason to send.
//
// Nothing here reads what they say: the relay reads the options receive-pack
// hands the hook, which it accepted (session.go); other bytes are no option.
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

// commandsRefusal is the sentence a push is refused in before its work when its
// commands are more than one push may carry (gitlet.Proposal.ValidateCommands),
// "" when they are not. They are measured as they will be proposed: each ref
// in its recorded form (gitlet.RecordedRef), pre's refusals applied, under the
// admitted principal and gitlet.ViaGit, which the entry carries. The width is
// said in sizes with what to do; any other rule (a ref named twice, a command
// that moves nothing) in the domain's own words.
func commandsRefusal(adm admission, node string, sec section, pre map[string]answered) string {
	if len(sec.Commands) == 0 {
		// Lines that parse as no command: receive-pack dies on them in its own
		// words (readCommands), and nothing is proposed.
		return ""
	}
	cmds := make([]gitlet.Command, len(sec.Commands))
	for i, c := range sec.Commands {
		cmds[i] = gitlet.Command{Ref: gitlet.RecordedRef(c.Ref), Old: c.Old, New: c.New}
		if a, ok := pre[c.Ref]; ok {
			cmds[i].Result, cmds[i].Reason = gitlet.ResultNG, a.Reason
		}
	}
	// WrittenFor too: its committer stamps it on a seat's entry
	// (admission.writtenFor).
	err := gitlet.Proposal{Principal: adm.principal(), WrittenFor: adm.writtenFor(), Via: gitlet.ViaGit,
		Node: node, Commands: cmds}.ValidateCommands()
	var wide *gitlet.TooWideError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &wide):
		return fmt.Sprintf("this push's ref updates take up to %s of its record, past the %s one push may; "+
			"push them in parts (git push origin <some refs>)", humanBytes(int64(wide.Bytes)),
			humanBytes(int64(wide.Room)))
	}
	return "this push's command list is not one the engine takes: " +
		strings.TrimPrefix(err.Error(), gitlet.ErrInvalidProposal.Error()+": ")
}

// recordedCommands is cmds as a proposal carries them, each ref in its
// recorded form (gitlet.RecordedRef), and the ref git named each recorded form
// for, to answer git with ([withRefs]).
func recordedCommands(cmds []gitlet.Command) ([]gitlet.Command, map[string]string) {
	out := make([]gitlet.Command, len(cmds))
	named := make(map[string]string, len(cmds))
	for i, c := range cmds {
		c.Ref = gitlet.RecordedRef(c.Ref)
		named[c.Ref] = cmds[i].Ref
		out[i] = c
	}
	return out, named
}

// withRefs is cmds with each recorded ref replaced by the ref git named: git
// matches a status to its command by ref name, and a row holds a refused ref
// in its bounded form.
func withRefs(cmds []gitlet.Command, named map[string]string) []gitlet.Command {
	out := make([]gitlet.Command, len(cmds))
	for i, c := range cmds {
		if ref, ok := named[c.Ref]; ok {
			c.Ref = ref
		}
		out[i] = c
	}
	return out
}

// parseCommand reads a line's `<old> <new> <ref>` (to its NUL) as
// queue_command does: two ids of hex in either case (wireOID lowercases them,
// as git spells them to the hook), each followed by a space, then the rest of
// the line as the ref, whatever it holds. A ref of any length is a command:
// whether the engine takes it is a refusal (bad_ref, gitlet.StatelessRefusal)
// answered in git's report, never a condition of being a command: else a ref
// git accepts and the engine refuses would be no command, its line's
// capabilities unread, and a push of it alone read as git's probe. An empty ref git refuses itself (`funny refname`);
// the receiver refuses it bad_ref.
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
