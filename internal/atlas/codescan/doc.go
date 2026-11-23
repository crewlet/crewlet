// Package codescan reads a repository's deploy and dependency files for the
// atlas, on Gitlet and on a forge Gitlet does not mirror it from: what its
// default branch declares the company runs (services, the databases, queues
// and buckets beside them, the vendors its code calls) and how they relate,
// by exact keys only. ADR-0133 records the decision, and ADR-0136 the forges'
// half.
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
// operators, providers); one off a table says nothing. [Parse] is pure and
// never fails, and a parser's panic is an unparsed file, never the node's:
// the bytes are anybody's with a push.
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
// [Rules] versions what the parsers read: a build that reads more or
// differently raises it, and every repository is read again though its tip
// has not moved.
//
// # A forge's repositories are read by the same reader
//
// A GitHub or GitLab repository Gitlet does not mirror is read through its
// forge (ADR-0136): [Forges] is the collect duty's [collect.CodePass], and a
// forge's source lists a tree and serves a blob ([collect.CodeHost]). Which
// file is listed, read or only counted, and that half a commit is never
// published, is one function for both hosts ([reader]); the list is held
// again where a forge is asked for a file's bytes, and the bytes are held to
// the object the tree listed. No event is subscribed: a pass asks each
// repository where its default branch is and reads only one that moved,
// within bounds of files, bytes and trees a pass, recording what it cost.
// Every standing shares one bucket, each filed under the host it is read
// from (coord.AtlasCodeScanKey): Gitlet's by repository id ([CoordStore]),
// a forge's by voice beside the forge's own ([ForgeStore]), so each reader
// lists its own host's alone, and a repository's pass reads its own by key.
//
// # What is not scanned
//
// A repository only its unit may read is never scanned: what a scan records
// is on the company's atlas log, whose history every holder of the atlas
// reads. A scratch repository, an archived one and one being imported are
// not what the company runs on. What an unscanned repository's files said
// before is withdrawn.
package codescan
