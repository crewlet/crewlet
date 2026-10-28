// Package codeindex is THE CODE HOST'S TRIGRAM INDEX, AS PURE FUNCTIONS OVER
// BYTES: which runs of three bytes each blob holds, the two files a
// repository's index is kept in, the plan of what a regular expression's
// matches cannot do without, and the query a person or a seat writes — so a
// code search narrows a repository's files to the few that can match before
// any of them is read. The cache owns the files (an incarnation's
// crewlet/codeindex/) and code/query owns git; this package reads and writes
// neither, so every rule here is decided on values, and a test of it needs no
// repository.
//
// # Keyed by blob
//
// A posting names a BLOB, by its object id — never a path, never a commit. A
// blob is the same bytes in every commit and at every path that holds it, so
// a new tip indexes only the blobs it introduced, and a compaction or a
// re-clone that changes every pack changes nothing here. Which postings count
// is a TIP MAP's to say: the tip's every file and the blob at it (tipmap.go).
// Blobs leave a repository's index when a merge keeps only those its tip
// still names (MergeSegments), which is also how its SEGMENTS (segment.go) —
// one per batch of new blobs — stay few.
//
// The obvious alternative, postings by path at a commit, re-indexes every
// file a rename or a new tip touches and holds one list a path for blobs the
// index already has; trigram tables in the replicated estate were rejected
// before that, as a row a posting on every data node and in every snapshot.
//
// # Over the Unicode lowering, with an escape
//
// Trigrams are taken over a blob's Unicode LOWERING (bytes.ToLower) — the
// comparison regexlit's prefilter makes for a (?i) literal — so one index
// answers a folded search and an exact one alike. A plain literal is looked
// up lowered too: a blob that holds it holds its lowering in its own, because
// lowering goes rune by rune and a literal is whole runes, so the lookup may
// admit a blob holding the literal in another case, which the confirm then
// drops, and never misses one. An ASCII-only lowering would be cheaper and
// wrong: (?i)été matches ÉTÉ, whose ASCII lowering is ÉtÉ.
//
// What no lowering sees through is a rune the regexp folds onto an ASCII
// letter without lowering to it — regexlit.HasEscape's, U+017F (ſ) alone
// today: (?i)status matches ſtatus, which lowers to itself. A blob holding one
// carries an ESCAPE flag, and every clause holding a folded literal admits it,
// as regexlit's Admits admits such a text. A plain clause still rules it out:
// an exact literal is in the blob byte for byte or not at all.
//
// Both are taken with the Unicode tables this build carries, which move with
// the Go toolchain: each Unicode version gives letters a case they lacked, so
// a blob filed under a capital's own bytes by one build is looked up by its
// lowering in the next. A segment outlives its build — kept across restarts,
// carried across incarnations by hard link, and never read again for a blob
// it holds — so it is STAMPED with the tables' version (unicode.Version), and
// a build carrying other tables refuses it as a file it did not write
// (ErrMalformed), which drops it and builds the index again. A tip map holds
// nothing the tables decide.
//
// # The plan, and what it refuses
//
// NewPlan reads the clauses off the expression's own syntax tree
// (regexlit.Required): an AND of clauses, each an OR of literals every match
// holds one of. A clause whose every literal is three bytes or more, in the
// form the index holds it, is looked up: a literal is a blob holding each of
// its trigrams, a clause any of its literals, the plan every clause it can
// look up. A shorter literal holds no trigram at all, so its clause cannot
// rule a blob out and is left to the confirm. A pattern NO clause of which
// can be looked up is REFUSED rather than answered by reading every file —
// the cost a search budget exists to keep off a git node — and so is one that
// can match across a line (regexlit.LineLocal), because a search answers
// lines, confirms them one at a time, and would find no match of it there;
// and one whose program costs too much to run on a line (below).
//
// A clause is LOOKED UP AS ONE, never a literal at a time (lookup.go). Its
// literals share trigrams — an alternation crossed with a word holds the word
// in every literal, and an alternation of names holds their common parts in
// some — and the commonest trigrams have a segment's longest lists, so
// looking each literal up whole read a list once for every literal holding
// it, decoded each time into a buffer of its own. A clause's trigrams are
// sorted by how many of its literals hold them, and its lookup reads each
// list it names once: the blobs holding what every literal holds once for
// all of them, each literal admitting only blobs among those; a list some
// literals hold decoded once for those, as far as the furthest blob one of
// them needs; and a literal's own lists by it alone, into nothing, as far as
// what it keeps reaches.
//
// Candidates are CONFIRMED with Go's regexp, line by line, and only on a line
// that holds what every match needs (Plan.Confirm): an answer is RE2's
// whichever path offered the blob, the index or `git grep -F` over the plan's
// literals. A second dialect — ERE through `git grep -E` — would answer the
// same query two ways.
//
// # What a confirm may cost
//
// Go's regexp runs in time linear in its input, but the factor is the
// program: it may step every instruction at every byte, and a run of it
// takes no context and cannot be stopped part way. A counted repetition
// compiles to as many copies of what it repeats, so 400 bytes of pattern can
// be a hundred thousand instructions, one and a half milliseconds a byte of
// a line — some twenty-five minutes of a CPU over a minified file's one
// mebibyte line, on the git node every clone and push of the repository is
// served from. So the cost is bounded where it is decided, three ways. A
// pattern whose program is past MaxProgram instructions is REFUSED
// (ErrTooComplex), which nothing a person or a seat searches for comes near.
// Confirm runs the expression on a line only where the line's bytes times
// the program's instructions are within MaxLineWork (Plan.LineBytes); a
// longer line that could match is COUNTED, never run, and never answered as
// holding no match. And Confirm takes its caller's context and checks it
// between lines, so a search's budget holds to within one line's run, which
// MaxLineWork bounds to 0.22 s on the cores it was measured on.
//
// The obvious alternatives are worse. A bound on a line's bytes alone,
// whatever its pattern, would keep a literal search out of the minified files
// a short program reads whole within the same cost; and running the regexp in
// a goroutine a search abandons at its deadline leaves it burning the CPU.
//
// # Skipped
//
// A blob past MaxFileBytes, or binary by git's own test (Binary: a NUL in its
// first BinaryProbeBytes), is SKIPPED: recorded in its segment, so its next
// tip does not read it again, but never read for trigrams and never a
// candidate. It is the rule `git grep -I` falls back to, so the index and the
// grep path skip the same files, and the one code/query's file reads call a
// file binary by.
//
// # The files
//
// A segment and a tip map are each one immutable file, big-endian, with a
// SHA-256 trailer over every byte before it — the cache's run-trailer check,
// which holds a file this process did not write before it serves — and each
// open holds the file's sections to its length, so no read lands outside it.
// The formats are written out at the head of segment.go and tipmap.go.
//
// # The query
//
// ParseQuery reads leading repo:, path: and ref: filters and then the text,
// at most MaxQueryBytes; a Mode says how the text is read — literal, regex or
// whole word — and every mode is an RE2 expression under (?m), so the
// expression the confirm runs is the one the plan was made of. PathFilter is
// the grammar of a path:, a .gitignore line's without its negation.
package codeindex
