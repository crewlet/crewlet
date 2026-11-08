// Package codesearch answers a code search over every repository an asker may
// read, on the Gitlet nodes that hold them (M6-D14).
//
// # One repository is a read; more are one scatter
//
// A search naming ONE repository is a git read of it (code.GitSearch) through
// the keyed walk every read by object id takes: the repository's Gitlet nodes
// asked in their order, the walk moving on from a node that cannot answer,
// and the answer or the refusal of the node that does is the search's.
//
// A search of more — every `search: indexed` repository the asker may read, or
// those its repo: filters name — is ONE SCATTER, never a walk a repository:
// a walk each would be a request each, thousands for an unfiltered search of a
// company. Every Gitlet node serves topics.CodeSearch, and the ask carries an
// ASSIGNMENT TABLE — which node answers for which repository — because a
// scatter divides work by what it names, not by who receives it (every server
// of a subject sees every request). A repository is assigned to the FIRST node
// of its git order (estate.Router.GitOrders, one reading of who serves git for
// the whole table): its home, and for an indexed repository the first of the
// code.IndexHolders keeping its index. A repository its node did not answer —
// silent, unready, failing, or holding no key the request is signed under — is
// asked once more of the NEXT node of its order, in a second round: an indexed
// repository's runner-up is the second holder, which keeps an index too. No
// third round: a repository two nodes could not answer is named in the
// coverage, never chased across the fleet while its asker waits.
//
// # Each node searches as the asker, so the request is signed
//
// A serving node carries the request out AS THE ASKER IT NAMES: each
// repository is read through internal/code/query, which syncs the copy and
// JUDGES THE ASKER'S GRANT from that node's own chart, as every read by object
// id is judged. So the request is an instruction, and the scatter is a
// signedask.Channel ([Channel]): an unsigned one would let any client of the
// broker read any repository as any seat or person in the company. A request
// carries an id and a deadline inside its signed body, a node takes an id
// once, and a reply names the request it answers — internal/signedask's rules,
// in key domains of the scatter's own.
//
// # The merge is exact
//
// Each repository answers its first lines in the order of its paths' bytes and
// then their numbers, at most the search's limit of them; a node keeps the
// first limit of its repositories' lines by (repository name, path, line), and
// the asker the first limit of every node's. No line one node left out can
// come before a line another kept, so the answer is the first limit lines of
// the whole search — the same whichever node answers first, since a round
// assigns each repository to exactly one node and the merge is a sort. A
// repository that stopped before its limit — at its answer's bound or its
// budget — answered its first lines up to its last, and the lines after that
// are unread, so the asker keeps no line of any repository past that last
// (prefixEnd, from the outcome each node answers for each repository): every
// line of the search before the answer's last is in the answer.
//
// # A page continues after its key
//
// A search asked AFTER a [Key] — the repository, commit and line a page of it
// was cut at — answers the lines after it and counts its limit among those
// alone: no repository before the key's is asked, and the key's own is read
// at the key's commit, its node asking git for no file before the key's path
// and dropping every line at or before the key's before its limit counts it.
// So a page of a long search costs the lines from its place on, never a whole
// search again, and continues the file the page before was cut in at the
// commit it was read at. The rest of its order is read as it stands then: a
// repository after the key has had no line answered yet.
//
// A MERGE HOLDS THE LIMIT, NEVER THE ANSWERS: each answer is folded in as it
// arrives and cut there, so a node assigned thousands of repositories holds
// the first limit lines and the answers of the reads in flight, not every
// answer until the last returns. And a node reads its repositories in the
// order of their names, so once its first limit lines are all of repositories
// named before the next, that one cannot reach the answer and is passed over
// unread: a search for a common word reads the repositories that fill the
// limit, not the company. Whether a repository was read or passed over turns
// on how the reads interleaved, so the coverage names every repository after
// the last line kept as past the limit (PastLimit) by its name alone.
//
// # What was not searched is said
//
// Every answer carries a [Coverage]: how many repositories answered, and each
// one that did not, by why — a node building the index (Building), a node
// behind the log or the company's configuration (Stale), a node that did not
// answer (NotAnswering, naming the node and the repositories), a grant the
// node's chart refuses (NotPermitted), a ref the repository does not hold
// (NoRef), every line of it past the answer's limit (PastLimit) — beside the
// grep repositories an unfiltered search leaves out
// (GrepNotNamed) and the indexed ones read with git grep (NotIndexed). A
// search that could not reach part of the company never reads as one that
// found nothing there.
//
// # What a search may cost
//
// The text is bounded as knowledge search's is (codeindex.MaxQueryBytes, 400
// bytes, refused and never cut). `search: grep` repositories are read only
// when a repo: filter names them, at most [MaxGrepRepos]; a filter's names
// are read beside each other, [ResolveReads] at once, so their linearizable
// reads join one barrier a wave, and a filter past the grep bound is refused
// as their rows arrive, the rest unread. A round waits at
// most [RoundBudget] and ends as soon as every node it asked has answered; a
// node's searches together read at most [ScatterReads] of its repositories at
// once — all but one unit of its seat git-work share, which each read's git
// takes, so a seat's ordinary read always finds one a search cannot hold —
// each under the search's own budget (code.SearchBudget).
package codesearch
