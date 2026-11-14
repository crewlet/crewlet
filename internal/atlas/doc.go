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
// address, never an identity, and a person's "not the same" always wins.
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
package atlas
