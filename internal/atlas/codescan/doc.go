// Package codescan reads a Gitlet repository's deploy and dependency files
// for the atlas: what its default branch declares the company runs (services,
// the databases, queues and buckets beside them, the vendors its code calls)
// and how they relate, by exact keys only. ADR-0133 records the decision.
//
// # Declared in code is a suggestion
//
// Every claim is made in the repository's own voice ([atlas.CodeSource]),
// whose confidence is inferred: shown as LIKELY, never confirmed, because a
// file says what somebody meant to run and not what runs. A person's confirm
// makes a claim theirs. The voice sets no fact of meaning and no owner, and
// denies nothing: a file that stops declaring something withdraws what it
// said, and whoever else holds the entry still does.
//
// # A closed list, and only what a parser declares a field for
//
// Only a file on the closed list is ever read (internal/deployfiles), and
// Gitlet itself refuses a read of any other. A parser decodes into closed
// types: an environment block, a command line, a secret's data and a
// dependency's version have no field to land in, so they are never decoded.
// What is decoded reaches a claim only through the vocabulary's grammars
// ([collect.Builder]), which refuse a credential-shaped key or value and
// count it. A name is matched against closed tables ([vendorOf], engines,
// operators, providers); one off a table says nothing.
//
// # Read when the default branch moves, and only what changed
//
// A [Scanner] is a worker of the repository's home loop
// (internal/gitlet/home): a record on the repository's subject of the code
// log nudges it, and the home's backstop lists every repository whose tip is
// not the one last read ([Scanner.Due]). No clock reads a file. One pass
// lists the tip's files in ONE git read, reads at most [MaxReadsPerPass]
// files whose blob it does not already hold a reading of, and publishes only
// once every listed file is read, so the atlas never holds half a commit.
//
// # Who has to agree on each piece of state
//
// What the files declared: every data node, identically, so it is records on
// the atlas log, written through the log's read record as an integration's
// read is. Each repository's standing (the tip read whole, what it listed,
// read and refused): the whole company, current value only, so it is a
// record in the coordination store ([CoordStore]). The reading of each blob:
// this node alone, and only while it runs, so it is memory ([readings]): a
// node that lost it reads a repository's files once more at its next move.
//
// # What is not scanned
//
// A repository only its unit may read is never scanned: what a scan records
// is on the company's atlas log, whose history every holder of the atlas
// reads. A scratch repository, an archived one and one being imported are
// not what the company runs on. What an unscanned repository's files said
// before is withdrawn.
package codescan
