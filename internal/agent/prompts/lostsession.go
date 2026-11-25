package prompts

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
)

// lostSessionHeader titles what a brief says of the session its run does not
// continue, and lostNewerHeader of the newer one where the run continues an
// older session in its place ([LostSession.Newer]).
const (
	lostSessionHeader = "## The session this run does not continue"
	lostNewerHeader   = "## The newer session this run does not continue"
)

// KeyLostSession keys the section [LostSessionSection] is a brief's.
const KeyLostSession = "lost_session"

// LostSession is what a brief tells of the session its run does not continue:
// the engine's verdict in its own words, the newest summary its harness kept of
// it, and where `read_session` reads it. Each turn id is the engine's record,
// defused by the caller.
type LostSession struct {
	// Sentence is the verdict as every surface says it (executorkeep's
	// NotContinued.Sentence), NEVER ITS DETAIL, which quotes what refused (a
	// store's error, a box's output) and told in the engine's voice would sit
	// outside any fence. "" where the run lost nothing it can name.
	Sentence string

	// Newer says the run continues an older session in the lost one's place,
	// so the section is the continuation's, handed only a summary newer than
	// what it continues.
	Newer bool

	// Summary is the lost session's newest summary as the brief hands it, nil
	// for none; Unread says one is kept that could not be read for this brief.
	Summary *HandedSummary
	Unread  bool

	// Read is the turn whose kept session `read_session` reads, "" where none
	// is kept or the run's surface carries no such tool; ThisTurn says it is
	// this turn's own, kept only until the run first saves its own, and Fire
	// that the conversation is a schedule's, told in its words.
	Read           string
	ThisTurn, Fire bool
}

// HandedSummary is a session's newest summary as a brief hands it: its kept
// end within the hand-over's bound (internal/agent/runner's handOverBytes),
// fenced as the seat's own past session where it is rendered.
type HandedSummary struct {
	// Text is the summary, the harness's model's words from what others
	// wrote; Seat the seat whose run kept it, which its fence names.
	Text string `untrusted:"fence"`
	Seat string

	// Turn is whose session it summarises, ThisTurn that it is this turn's;
	// At when the box read that found it kept it.
	Turn     string
	ThisTurn bool
	At       time.Time

	// Bytes is the whole summary's size, and Cut says Text is its last part
	// alone; KeptCut that the summary kept was itself cut, so no reader holds
	// the rest. Rest is the turn `read_session` reads the rest under, "" for
	// none.
	Bytes   int64
	Cut     bool
	KeptCut bool
	Rest    string
}

// LostSessionSection is the section [KeyLostSession] keys, "" where there is
// nothing to say: no verdict and no kept session to read. ONE SECTION, IN THE
// USER MESSAGE, after the endings and what became of the run before and before
// the round's calls, so those read as what came after the summary. A summary is
// handed fenced under b ([provenance.ExecutorSession]), since its harness's
// model wrote it from tool output and people's words: an account to check, never
// instructions.
func LostSessionSection(l LostSession, b provenance.Boundary) string {
	if l.Sentence == "" {
		return keptUnnamed(l)
	}
	said := nextTurnWordsFor(NextTurn{Fire: l.Fire})
	header := lostSessionHeader
	if l.Newer {
		header = lostNewerHeader
	}
	parts := []string{header + "\n" + l.Sentence}
	switch s := l.Summary; {
	case s != nil:
		parts = append(parts, summaryLead(*s, l.Newer, said), provenance.ExecutorSession(s.Seat).Fence(b,
			strings.TrimSpace(s.Text)))
	case l.Unread:
		parts = append(parts, "Its newest summary is kept, and could not be read for this brief: what it did is"+
			" told here only by the engine's record.")
	case !l.Newer:
		parts = append(parts, "No summary its harness wrote of it was kept: what it did is told here only by"+
			" the engine's record.")
	}
	if l.Read != "" && !l.Newer {
		parts = append(parts, readLine(l, said))
	}
	return strings.Join(parts, "\n")
}

// summaryLead introduces s: whose session it summarises and when it was kept,
// that it is an account to carry the work on from and check, what went with the
// box, and where it is cut.
func summaryLead(s HandedSummary, newer bool, said nextTurnWords) string {
	whose := "turn `" + s.Turn + "`'s session"
	if s.ThisTurn {
		whose = "this " + said.turn + "'s session"
	}
	lead := fmt.Sprintf("Its harness summarized the work as it went. The newest summary it kept, of %s at %s, is"+
		" below: the session's own account of the work up to then.", whose, s.At.UTC().Format(time.RFC3339))
	if newer {
		lead = fmt.Sprintf("Its harness summarized that work as it went. The newest summary it kept, of %s at"+
			" %s, is below: an account of work the session this run continues does not hold.", whose,
			s.At.UTC().Format(time.RFC3339))
	}
	lead += " Carry the work on from it rather than starting over, and check what it reports against the" +
		" repository and the calls told here before relying on it: what that session had not committed and" +
		" pushed went with its box, and the paths it names were that box's."
	if !s.Cut && !s.KeptCut {
		return lead
	}
	lead += fmt.Sprintf(" It is cut: only its last %d bytes of %d are here", len(strings.TrimSpace(s.Text)), s.Bytes)
	switch {
	case s.KeptCut:
		return lead + ", and the rest was not kept."
	case s.Rest != "":
		return lead + "; `" + toolname.ReadSession + "` reads it under turn_id `" + s.Rest + "`, and `match`" +
			" finds a word in the rest."
	}
	return lead + ", and no tool here reads the rest."
}

// readLine is where `read_session` reads the kept session l names. THIS
// TURN'S IS KEPT ONLY UNTIL THIS RUN FIRST SAVES ITS OWN, so the run is told to
// read it before then; no time is given, which pacing or a failed save would
// make false.
func readLine(l LostSession, said nextTurnWords) string {
	if l.ThisTurn {
		return "Read what this " + said.turn + "'s earlier launches did with `" + toolname.ReadSession +
			"`, turn_id `" + l.Read + "`, before this run first saves its own session, which replaces it;" +
			" `match` finds a word in it."
	}
	return "Read the session itself with `" + toolname.ReadSession + "`, turn_id `" + l.Read + "`; `match`" +
		" finds a word in it."
}

// keptUnnamed is the section of a run that lost nothing it can name beside a
// kept session it does not continue (a round lost a second time): that session
// is there to read, which its model cannot otherwise know. A schedule's fire is
// told in its words, as [BuildNextTurn] does. "" where none is kept to read.
func keptUnnamed(l LostSession) string {
	if l.Read == "" {
		return ""
	}
	said := nextTurnWordsFor(NextTurn{Fire: l.Fire})
	whose, read := "This "+said.whole+"'s last "+said.turn, ". Read what it did"
	if l.ThisTurn {
		whose = "This " + said.turn + "'s earlier launches"
		read = ", kept until this run first saves its own. Until then, read what they did"
	}
	return lostSessionHeader + "\n" + whose + " worked in a session this run does not continue" + read +
		" with `" + toolname.ReadSession + "`, turn_id `" + l.Read + "`; `match` finds a word in it."
}
