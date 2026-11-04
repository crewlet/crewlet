// Package executorread reads one of a seat's kept executor sessions
// (ADR-0073): its turn's row on the executor log, the sealed object that row
// names, and a page of the transcript the object holds — and says what the
// read came to in one set of words, for every surface a session is read
// through.
//
// # One read, so one set of words
//
// A session is read where a person reads a seat's trail
// (internal/api/queries), and every reader of it is owed the same account:
// a row the log no longer holds is said one way, a row naming no session says
// why in the domain's own sentence (executordomain.Session.NotKept), and an
// object that does not read is one of five states, each in the engine's own
// words ([TranscriptStates]). Two copies of those words are two surfaces that
// tell a person and a model different things about one session, so the read is
// here and every surface calls [Read].
//
// # The cursor names the object, and the count of entries
//
// A page's cursor is `<object key>.<count>.<ordinal>` ([Cursor]): the object
// the page was read from, the version of what this build counts as a
// transcript's entry (internal/sandbox/executor's TranscriptVersion), and the
// ordinal of the entry the next page starts at. The object is in it because a
// live turn's row moves to a newer object with every capture, and the count
// because a rolling upgrade serves two builds side by side: a page read from
// one archive, or counted by one build, and continued in another would skip or
// repeat entries with nothing saying so. Such a cursor is refused
// ([ErrCursor]), saying to read the transcript again from its start.
//
// # Why a package of its own
//
// The read needs the log's row — its object, the reason it names none, and the
// failure that says its object does not open (executordomain.ErrUnreadable) —
// and the runner's knowledge of the archive (internal/sandbox/executor's
// ReadTranscript). The runner's package links no state log, as the sandbox
// whose runners it holds does not (its dependency gate), and the log's domain
// is no place to decode a box's archive. So the two meet here, below every
// surface that reads a session and above both.
package executorread
