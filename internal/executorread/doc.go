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
// words ([TranscriptStates]). A row naming the session of a turn that has not
// ended is read as the session it is, in progress, and a page the row's newer
// capture no longer continues is refused saying the turn has not ended and its
// runs have kept its session again — what the row says, and never that a run
// saved it. Two copies of those words are two surfaces that tell a person and
// a model different things about one session, so the read is here and every
// surface calls [Read].
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
// # A read holds nothing, so a release is said as one
//
// Reading a session writes no record and keeps no session past what the seat's
// budget and the retention give it: a hold taken by a read would be a write
// every reader originates, and it would keep the budget from releasing the
// reader's own next capture. So the session a page reads can go between the
// read of its row and the end of its object's stream — a later turn's end
// absorbs it, the seat's budget releases it, the end of a turn that ran out of
// its model's context in it retires it, the retention deletes it, or its turn
// captures it again — and the collector takes the object once no row names
// it. Read alone, a collected object is one the store no longer holds,
// and one taken while it streams one whose bytes are not the sealed ones
// (executordomain.ErrUnreadable), so the failure alone would say a release as a
// loss, or as corruption. Instead a read whose object fails reads the turn's
// row once more, at `linearizable` ([ReadAgain]), and the page says what the
// row says now ([Read]); only where the row still names the object is the
// failure the store's or the archive's own. What the read already handed out
// stands: every byte of it came from a chunk that authenticated.
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
