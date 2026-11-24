package executor

import (
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// boxFilesMark opens each file in the answer of [boxFilesCommand],
// followed by the file's size and path.
const boxFilesMark = "crewlet-file "

// boxFilesCommand is the shell printing, for each of files the box's user
// can read as a regular file, the mark, its size and its path on a line, then
// its bytes: the box's own view of a harness's settings, outside its home too
// (/etc), which [sandbox.Sandbox.ReadFile] does not reach on a box on the
// engine host. A file that user cannot read is not printed, as a harness
// running as that user cannot read it either. A glob that matches nothing
// stays a word no file is named.
func boxFilesCommand(files []string) string {
	words := make([]string, len(files))
	for i, f := range files {
		if strings.Contains(f, "*") {
			words[i] = f
			continue
		}
		words[i] = shellword.Quote(f)
	}
	return `for f in ` + strings.Join(words, " ") + `; do [ -f "$f" ] && [ -r "$f" ] || continue; ` +
		`n=$(wc -c < "$f") || continue; printf '` + boxFilesMark + `%s %s\n' $n "$f"; cat -- "$f"; done; exit 0`
}

// boxFileRead is one file of a [boxFilesCommand] answer.
type boxFileRead struct {
	path string
	raw  []byte
}

// boxFilesParse parses out, the answer of [boxFilesCommand], into
// its files in order. A section cut short ends the answer there.
func boxFilesParse(out string) []boxFileRead {
	var files []boxFileRead
	for {
		at := strings.Index(out, boxFilesMark)
		if at < 0 {
			return files
		}
		out = out[at+len(boxFilesMark):]
		head, rest, ok := strings.Cut(out, "\n")
		if !ok {
			return files
		}
		size, name, ok := strings.Cut(head, " ")
		n, err := strconv.Atoi(size)
		if !ok || err != nil || n < 0 || n > len(rest) {
			return files
		}
		files = append(files, boxFileRead{path: name, raw: []byte(rest[:n])})
		out = rest[n:]
	}
}
