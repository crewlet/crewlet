package sandbox

import "io"

// A HARNESS'S OWN SESSION, CARRIED BETWEEN BOXES.
//
// A run's harness keeps its conversation in its box — Claude Code as a transcript
// under its configuration directory, OpenCode in its database — and the box
// is reclaimed when its turn settles. So a session that is to be continued in
// another box leaves the one it ran in PACKED, as one archive the box's run
// wrote when its harness exited ([Result.Captured]), and enters the next one as
// that archive, written into the box before its run starts
// ([RunRequest.Carried]) and restored there by the run's own script before its
// harness takes the session up. What a harness's archive holds, and what it never holds
// — a login, the harness's own settings, a checkpoint of files — is its runner's to
// say (internal/sandbox/executor), and so is whether a box can continue one
// at all ([CarryCheck]).

// SessionArchive describes a harness's session packed into one archive: by
// the box its run ended in, or as it is carried into another.
type SessionArchive struct {
	// Harness is the runner whose harness wrote the session ("claude-code",
	// "opencode"): a session is continued only by the harness that wrote it.
	Harness string

	// SessionID is the session's id, in its harness's own form.
	SessionID string

	// Format is the version of the archive's layout, its runner's constant
	// for its harness: a runner restores only the format it packs, and refuses
	// any other rather than reading it as one.
	Format int

	// Version is the release of the harness that wrote the session, as its
	// `--version` gave it ("2.1.310"), which decides whether another box's
	// harness can continue it ([CarryCheck]); "" where the run that wrote it
	// recorded none this build can read.
	Version string

	// Size is the archive's length in bytes, as its box reported it when it
	// was described — read before its bytes are, so a reader can refuse one
	// too large to keep before reading any of it.
	Size int64

	// Path is where the box holds the archive, in the box's own paths. Set on
	// a capture ([Result.Captured]); a carried session's is its runner's to
	// choose.
	Path string
}

// CarriedSession is a session archive handed to a run to continue
// ([RunRequest.Carried]): what it is, and its bytes, read once by the run's
// start and never closed by it — the caller opened them and closes them.
type CarriedSession struct {
	Archive SessionArchive
	Body    io.Reader
}

// CarryCheck is a box's answer to whether a session carried from another box
// can be continued in it (executor's Runner.CheckCarry), with the release of
// the box's harness it read to decide, which the run's record carries.
type CarryCheck struct {
	// Version is the box harness's release as its `--version` gave it
	// ("2.1.310"); "" where it printed nothing this build can read, which is
	// taken for a current release.
	Version string

	// Continues says the session can be continued here.
	Continues bool

	// Why says, in a person's words, why it cannot; "" where it can.
	Why string
}
