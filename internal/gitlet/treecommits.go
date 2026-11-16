package gitlet

// A DIRECTORY'S LAST COMMITS ([GitTreeCommits]): each entry's commit as `git
// log -1 -- <entry>` names it, an estate request that moves no RecordVersion.

// TreeCommitsQuery is the commit that last changed each entry of a directory
// (GitTreeCommits), a page at a time from GitQuery.Cursor, which is the
// listing's own entry cursor: a page begins at the entry a listing's does.
type TreeCommitsQuery struct {
	// Rev is a commit's id (or a tag's naming one), whole or abbreviated.
	Rev string `json:"rev"`
	// Path is the directory from the repository's root, empty for the root;
	// PathB64 its exact bytes where it is not UTF-8, as TreeQuery's.
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
}

// validate refuses a read no Gitlet node answers ([ErrInvalid], naming the
// field): a revision that is no object id, and a path [GitPath] refuses.
func (q TreeCommitsQuery) validate() error {
	if err := validRev("tree_commits.rev", q.Rev); err != nil {
		return err
	}
	_, err := GitPath("tree_commits.path", q.Path, q.PathB64, true)
	return err
}

// TreeCommitsAnswer is one page of a directory's last commits: its entries
// from the cursor on, in the listing's order, and each commit they name once.
type TreeCommitsAnswer struct {
	// Tree is the directory's tree; Path the directory (PathB64 per WirePath).
	Tree    string       `json:"tree"`
	Path    string       `json:"path,omitempty"`
	PathB64 string       `json:"path_b64,omitempty"`
	Entries []TreeCommit `json:"entries"`
	// Commits is each commit Entries name, once, in the order they first
	// name it, as a history lists it.
	Commits []LogCommit `json:"commits"`
	// Total is how many entries the directory holds, on every page.
	Total int `json:"total"`
	// OutOfTime marks a page the read's budget of git ended early: before an
	// entry it did not answer, or after its first, answered OutOfTime alone.
	OutOfTime bool `json:"out_of_time,omitempty"`
}

// TreeCommit is one entry of a page.
type TreeCommit struct {
	// Path is the entry's from the repository's root (PathB64 per WirePath),
	// as the listing names it.
	Path    string `json:"path"`
	PathB64 string `json:"path_b64,omitempty"`
	// Commit is the commit that last changed a file of it; empty where git
	// names none, a directory that holds no file and never did along its
	// history, and where OutOfTime is set.
	Commit string `json:"commit,omitempty"`
	// OutOfTime marks a page's first entry whose commit the read's budget of
	// git ran out before finding; the page ends after it.
	OutOfTime bool `json:"out_of_time,omitempty"`
}

// Cut reports a page the read's budget ended (OutOfTime): the machine's and
// the moment's, so never kept and never answered as unchanging.
func (a *TreeCommitsAnswer) Cut() bool { return a != nil && a.OutOfTime }
