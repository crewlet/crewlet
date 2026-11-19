// Package atlas is the vocabulary of the company's atlas, the map and catalogue
// of everything the company runs on, and the one function that decides what it
// says ([Fold]). It holds values and rules only: it imports no store, no
// coordination, no state log and no API, so it can be read by every layer and
// written by none (TestTheAtlasImportsNoWritePath).
//
// # Claims, computed or recorded
//
// Every fact is a [Claim]: about one entry, from one [Source], as of one
// instant. What the engine already keeps (the company settings, read only
// through the redacting copy, and the engine's own records, Gitlet's rows
// first) is COMPUTED when the atlas is read and never stored, so a fact the
// settings declare cannot drift from its declaration and needs no writer. What
// only somebody can tell the engine (a person, an agent in a named turn, an
// integration's read, a repository's deploy and dependency files) is
// RECORDED, on a log of its own. Both are claims, and one fold reads them.
// ADR-0126 records the decision.
//
// # A closed vocabulary, typed values
//
// Kinds of entry, lanes, vendors, capabilities, relations, facts and value
// types are closed sets, each a named type with Valid(), growing only through a
// change with its tests. A value is never "a string": it is one of a closed set
// of [ValueType]s, each with a grammar that answers the canonical form or a
// refusal naming the rule. There is no type for a connection string, a command
// line, a file body, an environment value or a vendor's raw answer, so none of
// them has anywhere to go, and a value shaped like a credential is REFUSED
// ([ErrCredential]), never masked and kept: a masked value is a value nobody
// meant to record. An address on a network the public internet does not route
// is kept, and marked Private by the engine's one reading of a private host
// ([config.MCPPrivateHost], which also decides whether plain http may reach a
// tool server).
//
// # Identity
//
// An entry is its kind and its canonical natural key ([Ref]); its id is a
// name-based UUID over the two, so two writers that meet the same zone name the
// same entry without asking anybody. Two entries of one kind that share an
// exact key (a public address, a forge address, an account's own id) are one
// entry when the atlas is read: computed, so a wrong key rule is fixed in code
// with nothing to migrate. A key one source carries on two entries is a shared
// address, never an identity, an agent's claim merges nothing, and a person's
// "not the same" always wins.
// Similar names never merge anything.
//
// # Confidence is derived, never chosen
//
// A claim's [Confidence] is a function of its source and the evidence the
// engine checked, so no caller passes one in and no model can be steered into
// raising it. Six levels order the fold; a reader is shown three words. What
// a repository's files declare ([CodeSource], ADR-0133) is inferred, as what
// an agent read in a file is: likely, and never confirmed by the file alone.
//
// # The fold
//
// [Fold] is pure and deterministic: the same claims in any order give the same
// view. A source's newest claim on a fact stands for it, the most trusted
// source wins, a person outranks every inference, and a fact of meaning
// (purpose, environment, importance, the owner) is decided only by a person or
// the settings; an integration or an agent only suggests it. A disagreement is
// shown beside the value, as contested or disputed, and never settled in
// silence. An entry's state is read off its claims on every read, so no state
// outlives its condition.
//
// # Deny, dispute and what only an agent said
//
// An integration denies only the entry itself (its retirement, ADR-0129),
// never a fact or a relation, which it withdraws. An agent's denial is a
// [Dispute] and never wins (ADR-0132): resolve never takes it as a facet's
// winner, so an agent retires no entry and removes no fact, and the vocabulary
// refuses an agent's dependency beside its owner and its facts of meaning. A
// repository's files ([CodeSource]) are refused a fact of meaning, an owner, a
// denial and "not the same", so they never contest what was seen. [Vouched]
// leaves out whatever rests only on an agent's unbacked report; Select applies
// it unless a read asks for the unconfirmed, and every agent's read always.
// [Mentions] and [Ref.Identifiers] hold an agent's record to the cited answer
// by one rule: a value whole, with no letter, digit, hyphen or underscore
// beside it.
//
// # Facts per host
//
// A fact may be one of each host of a merged entry ([Spec.PerHost], ADR-0131; a
// repository's visibility is the first): the fold keeps one value a member,
// shown with its member (FactView.Of) and held to that host's own words
// ([Fact.WordsOn]), so a mirror's two hosts are never two voices on one fact.
// It is not a way to quiet a real disagreement, which is one host's voices
// differing. A relation runs from what relies to what is relied on, between
// the kinds its spec lists, and falls in one of three families
// ([Relation.Family]): the map answers the structural ones or, asked, all.
//
// # Joins and signals
//
// What the records say beyond facts (agents holding accounts, use, signals) is
// joined, never claimed ([Join]). Each signal names its audience, so the atlas
// never shows what another screen withholds, and "not used recently" is said
// only of a channel read in full. Two signals are read off the claims at every
// join and never stored: a monitor's state on it and on what it watches, and a
// failing last pipeline result on it and on what it deploys (pipeline_failing,
// a warning, since a failed run leaves what ran before it in place). What waits
// for a person (View.Review) is likewise computed on the read and stored
// nowhere, so it never outlives its cause: it carries each agent's newest
// report (the seat, the turn, the cited call's tool and evidence), the voice
// named by its newest claim and never by the order a map was walked in;
// ReviewQuery.ContradictedFor narrows it to what contradicts one person's own
// word, the one thing their Inbox is told of.
//
// # Reads
//
// Reads are pure too (query.go): a list pages on a keyset cursor (50, at most
// 500), the map closes every lane and its groups largest first past 2,000
// entries (collapsed), a trace stops at 6 hops and 200 entries, and facets are
// each counted over every other filter. For leaves out every signal the reader
// is not in the audience of, so the API and the tools narrow alike. A log line
// is shown only with its entries (View.Logged, View.Line): never on or naming
// an entry the reader's atlas does not hold, an unconfirmed one a read leaves
// out, or a Gitlet repository or check Gitlet's records did not list to this
// reader. Summaries.Of says what a screen would open every entry for: the
// owner a person or the records name (never a suggested one), the purpose, and
// how many entries it relies on and rely on it, counted between the entries
// the read shows so a count never tells of a retired or hidden one, and a
// Reason when it is not quiet. Every sentence the package writes (Entry.Reason,
// a log Line's text) is the engine's own: closed words and a count, never a
// name, label, value or alarm text, which ride beside it in typed fields, so do
// not improve one by putting a name in it.
//
// # An agent's reach
//
// An agent reads an internal address only within its own reach (reach.go,
// ADR-0109): [Within] drops every private value of an entry its access does
// not reach, and an entry whose key is itself private with every relation to
// it, and counts what it dropped. [Named] is the entries a text names exactly
// (a whole word, case folded, at least MinNamed characters), never a fuzzy
// match, so a prompt is handed only what the work names. [Writers] is who wrote
// what entries carry; a vendor's listing is reported apart because it names no
// writer and is read as an outside party's. A server a DNS record points at is
// keyed addr/<ip> ([AddressHostKey]), so it merges with an SSH server declaring
// the same public address, and a certificate by its vendor's own identifier
// (<vendor>/<id>). The patterns and tables are built at first use, since
// Gitlet's push hook pays every linked package's init.
package atlas
