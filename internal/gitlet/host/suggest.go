package host

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/toolname"
)

// An apply_suggestions' build puts findings' suggestions in place of their
// anchored lines in the files at the change's head, ALL OR NONE, each placed
// by the rule a finding is shown by (gitlet.Reanchor), so it lands exactly
// where a reader was shown its finding. A suggestion read in the head's own
// blob is where it was read; every other is placed by ONE `git diff-pairs`
// over the distinct (read, head) blob pairs, its hunks fed to the placings as
// they stream (gitlet.Placing), so no diff is held. A file gone or not regular
// at the head, a hunk touching the lines, a binary pair or a file shorter
// than the lines (found by the splice) is OUTDATED, and overlapping placed
// lines an OVERLAP; either refuses the whole write, naming every finding. The
// commit, a commit_files' of one edit a file, reads each file through its
// splices as it streams (spliceStream): one forward pass, no file held whole.

// splice is one suggestion as it is put in a file: its finding, the lines of
// the head's file it replaces — placed, 1-based and inclusive — and its text.
type splice struct {
	finding    string
	start, end int
	text       string
}

// suggestedFiles is one edit of every file a suggestion rewrites, sorted; each
// is read through its splices, not from an edit's text.
func suggestedFiles(ss []gitlet.SuggestionOp) []gitlet.FileOp {
	var paths []string
	for _, s := range ss {
		if !slices.Contains(paths, s.Path) {
			paths = append(paths, s.Path)
		}
	}
	slices.Sort(paths)
	out := make([]gitlet.FileOp, len(paths))
	for i, p := range paths {
		out[i] = gitlet.FileOp{Op: gitlet.FileOpEdit, Path: p}
	}
	return out
}

// applySuggestions is an apply_suggestions' build: its files at the head, its
// suggestions placed in them and held apart, and the commit of the files read
// through their splices — or the write's refusal, outdated or overlapping.
func (b *building) applySuggestions(ctx context.Context, j judgement) (string, gitlet.EngineWriteResult, bool, error) {
	wr := b.wr
	files := suggestedFiles(wr.w.Suggestions)
	paths, err := b.paths(ctx, j.parent, files)
	if err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	var outdated []string
	for _, s := range wr.w.Suggestions {
		if o := paths[s.Path]; !o.file() {
			what := "gone"
			if o.present() {
				what = o.what()
			}
			outdated = append(outdated, fmt.Sprintf("finding %s's %s is %s at %s", s.FindingID, s.Path, what,
				shortID(j.parent)))
		}
	}
	if len(outdated) > 0 {
		return "", wr.changeRefused(gitlet.WriteSuggestionOutdated, outdatedSentence(outdated)), true, nil
	}
	spliced, outdated, err := b.place(ctx, paths)
	switch {
	case err != nil:
		return "", gitlet.EngineWriteResult{}, true, err
	case len(outdated) > 0:
		return "", wr.changeRefused(gitlet.WriteSuggestionOutdated, outdatedSentence(outdated)), true, nil
	}
	if overlaps := overlapping(spliced); len(overlaps) > 0 {
		detail := fmt.Sprintf("%s: nothing was applied — apply one of each pair, read the findings again and "+
			"apply the other", strings.Join(overlaps, "; "))
		return "", wr.changeRefused(gitlet.WriteSuggestionOverlap, detail), true, nil
	}
	return b.commitPlan(ctx, j, files, paths, spliced)
}

// outdatedSentence is an outdated apply's sentence, every finding named.
func outdatedSentence(outdated []string) string {
	return fmt.Sprintf("%s: nothing was applied; read the findings at the change's head with %s "+
		"and apply those still in place", strings.Join(outdated, "; "), toolname.CodeGetChange)
}

// place is each suggestion's placed lines in the head's file, by path and
// sorted by start, and every suggestion whose lines changed since they were
// read, named. diff-pairs is handed its pairs numbered rather than named, as
// internal/gitlet/query feeds the re-anchoring read; pairNamed reads them back.
func (b *building) place(ctx context.Context, paths map[string]object) (map[string][]splice, []string, error) {
	ss := b.wr.w.Suggestions
	placings := make([]gitlet.Placing, len(ss))
	of := make([]int, len(ss)) // each suggestion's pair, -1 for none
	type pair struct{ old, new string }
	var pairs []pair
	index := map[pair]int{}
	byPair := map[int][]int{}
	for i, s := range ss {
		of[i] = -1
		placings[i] = gitlet.PlaceLines(s.LineStart, s.LineEnd)
		head := paths[s.Path].id
		if head == s.BlobOID {
			continue
		}
		p := pair{s.BlobOID, head}
		n, ok := index[p]
		if !ok {
			n = len(pairs)
			index[p] = n
			pairs = append(pairs, p)
		}
		of[i] = n
		byPair[n] = append(byPair[n], i)
	}
	binary := map[int]bool{}
	if len(pairs) > 0 {
		var in bytes.Buffer
		for n, p := range pairs {
			fmt.Fprintf(&in, ":100644 100644 %s %s M\x00%d\x00", p.old, p.new, n)
		}
		cur := -1
		lines := newHeaderLines(func(line []byte) error {
			switch {
			case bytes.HasPrefix(line, []byte("diff --git ")):
				at, ok := pairNamed(line, len(pairs))
				if !ok {
					return fmt.Errorf("host: diff-pairs answered %q, which names no pair it was handed", line)
				}
				cur = at
			case bytes.HasPrefix(line, []byte("Binary files ")) && cur >= 0:
				binary[cur] = true
			case bytes.HasPrefix(line, []byte("@@ ")) && cur >= 0:
				h, ok := gitlet.ParseHunk(line)
				if !ok {
					return fmt.Errorf("host: diff-pairs answered %q, which is no hunk header", line)
				}
				for _, i := range byPair[cur] {
					placings[i].Hunk(h)
				}
			}
			return nil
		})
		left, err := b.wr.budget()
		if err != nil {
			return nil, nil, err
		}
		res, err := b.wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.wr.synced.Dir, Args: []string{"diff-pairs", "-z", "-p", "-U0", "--no-color", "--no-ext-diff",
				"--no-textconv"},
			Config: b.wr.s.o.Cache.ServeConfig(), Stdin: &in, Stdout: lines, Class: gitproc.ClassRead, Deadline: left,
		})
		if err == nil {
			err = lines.close()
		}
		if err = b.wr.failed(ctx, "diff of the files the suggestions were read in", res, err); err != nil {
			return nil, nil, err
		}
	}
	out := map[string][]splice{}
	var outdated []string
	for i, s := range ss {
		start, end, kept := placings[i].Placed()
		switch {
		case of[i] >= 0 && binary[of[i]]:
			outdated = append(outdated, fmt.Sprintf("finding %s's %s is a file git reads as binary, whose lines "+
				"nothing places", s.FindingID, s.Path))
			continue
		case !kept:
			outdated = append(outdated, fmt.Sprintf("finding %s's lines %d to %d of %s changed since they were "+
				"read", s.FindingID, s.LineStart, s.LineEnd, s.Path))
			continue
		}
		out[s.Path] = append(out[s.Path], splice{finding: s.FindingID, start: start, end: end, text: s.Text})
	}
	for _, p := range slices.Sorted(maps.Keys(out)) {
		slices.SortFunc(out[p], func(a, b splice) int { return a.start - b.start })
	}
	return out, outdated, nil
}

// pairNamed is the pair a "diff --git a/<n> b/<n>" header names, one of n.
func pairNamed(line []byte, n int) (int, bool) {
	a, b, ok := strings.Cut(strings.TrimPrefix(string(line), "diff --git "), " ")
	if !ok || !strings.HasPrefix(a, "a/") || b != "b/"+a[2:] {
		return 0, false
	}
	at, err := strconv.Atoi(a[2:])
	if err != nil || at < 0 || at >= n || strconv.Itoa(at) != a[2:] {
		return 0, false
	}
	return at, true
}

// overlapping names every suggestion whose placed lines overlap another's in
// its file, each sorted splice compared with the earlier one reaching
// furthest, so every finding in an overlap is named in at most one sentence
// each. Comparing neighbours alone misses 5-6 after 1-10 and 2-3: it overlaps
// only the first.
func overlapping(spliced map[string][]splice) []string {
	var out []string
	for _, p := range slices.Sorted(maps.Keys(spliced)) {
		s := spliced[p]
		reach := 0
		for k := 1; k < len(s); k++ {
			if s[k].start <= s[reach].end {
				out = append(out, fmt.Sprintf("findings %s and %s both suggest lines %d to %d of %s",
					s[reach].finding, s[k].finding, s[k].start, min(s[k].end, s[reach].end), p))
			}
			if s[k].end > s[reach].end {
				reach = k
			}
		}
	}
	return out
}

// headerWindow is how much of each diff-pairs output line placement keeps.
// Every line it acts on is a header ("diff --git a/<n> b/<n>", "Binary files
// …", or "@@ -a,b +c,d @@" plus at most the 80 bytes of function context
// xdiff appends), so 256 bytes holds each whole while a content line is never
// held whole, however long; gitlet.ParseHunk reads nothing past the closing @@.
const headerWindow = 256

// headerLines is a Cmd.Stdout handing each line of a patch to fn as it ends,
// cut to its first headerWindow bytes.
type headerLines struct {
	fn   func(line []byte) error
	line []byte
	err  error
}

// newHeaderLines hands each line, cut to its window, to fn — valid only during
// the call.
func newHeaderLines(fn func(line []byte) error) *headerLines {
	return &headerLines{fn: fn, line: make([]byte, 0, headerWindow)}
}

func (h *headerLines) Write(p []byte) (int, error) {
	if h.err != nil {
		return 0, h.err
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if room := headerWindow - len(h.line); room > 0 {
			h.line = append(h.line, chunk[:min(room, len(chunk))]...)
		}
		if i < 0 {
			break
		}
		if err := h.fn(h.line); err != nil {
			h.err = err
			return 0, err
		}
		h.line = h.line[:0]
		p = p[i+1:]
	}
	return n, nil
}

// close is the output's end: the last line, where it ended with no newline.
func (h *headerLines) close() error {
	if h.err == nil && len(h.line) > 0 {
		h.err = h.fn(h.line)
		h.line = h.line[:0]
	}
	return h.err
}

// spliceStream is a file read through an apply's splices into its new blob:
// a line in a splice's range is left out and the splice's text written once
// the range's last line is read, ending in a newline only if that line did;
// every other line passes through. Splices are sorted and apart, so it reads
// the file once and holds none of it, git's binary test (the first
// editBinaryHead bytes) looked at as they pass.
type spliceStream struct {
	out     io.Writer
	splices []splice
	next    int // the next splice the file has not passed
	line    int // the line being read, from 1
	open    bool
	seen    int // the bytes read, up to editBinaryHead
	binary  bool
	err     error
}

// newSpliceStream rewrites the file it is written into out, by splices.
func newSpliceStream(splices []splice, out io.Writer) *spliceStream {
	return &spliceStream{out: out, splices: splices, line: 1}
}

// inside reports whether the line being read is in the next splice's range.
func (s *spliceStream) inside() bool {
	return s.next < len(s.splices) && s.line >= s.splices[s.next].start && s.line <= s.splices[s.next].end
}

func (s *spliceStream) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n := len(p)
	if s.seen < editBinaryHead {
		look := p[:min(len(p), editBinaryHead-s.seen)]
		s.seen += len(look)
		if bytes.IndexByte(look, 0) >= 0 {
			s.binary = true
		}
	}
	if s.binary {
		return n, nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i+1]
		}
		s.open = true
		if !s.inside() {
			if _, err := s.out.Write(chunk); err != nil {
				s.err = err
				return 0, err
			}
		}
		if i < 0 {
			break
		}
		if err := s.ended(true); err != nil {
			s.err = err
			return 0, err
		}
		p = p[i+1:]
	}
	return n, nil
}

// ended is the end of the line being read — with its newline, or the file's
// unterminated last: the next splice's text written where it was the last
// line of its range.
func (s *spliceStream) ended(newline bool) error {
	if s.next < len(s.splices) && s.line == s.splices[s.next].end {
		text := s.splices[s.next].text
		switch {
		case newline && text != "" && !strings.HasSuffix(text, "\n"):
			text += "\n"
		case !newline:
			text = strings.TrimSuffix(text, "\n")
		}
		if _, err := io.WriteString(s.out, text); err != nil {
			return err
		}
		s.next++
	}
	s.line++
	s.open = false
	return nil
}

// Close is the file's end: its unterminated last line ended, and the apply's
// refusal of the file where it is one git reads as binary, or ends before a
// splice's lines.
func (s *spliceStream) Close(path string) (gitlet.WriteRefusal, string, error) {
	if s.err != nil {
		return "", "", s.err
	}
	if s.binary {
		return gitlet.WriteEditBinary, fmt.Sprintf("%s is a file git reads as binary at the head, whose lines no "+
			"suggestion replaces", path), nil
	}
	if s.open {
		if err := s.ended(false); err != nil {
			return "", "", err
		}
	}
	if s.next < len(s.splices) {
		short := s.splices[s.next]
		return gitlet.WriteSuggestionOutdated, fmt.Sprintf("finding %s suggests lines %d to %d of %s, which holds %d "+
			"lines at the head: nothing was applied", short.finding, short.start, short.end, path, s.line-1), nil
	}
	return "", "", nil
}
