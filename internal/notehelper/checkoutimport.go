package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

// rootImports is root's instructions as imports, for a spec whose harness
// reads its instruction files itself (checkoutSpec.ImportInstructions): each
// slot's first regular file as one `@<absolute path>` line, in slot order, its
// spaces written `\ `. Claude Code 2.1.310 and 2.1.330 then load each file and
// resolve its own relative imports against the checkout, as a session started
// there does (measured: a copied CLAUDE.md's resolved against the home). The
// file must stay inside the checkout, links followed only within it, and open
// as the same regular file without following a link; one that does not, or
// whose path an import cannot name, is left out, said on stderr. No limit
// applies: the harness reads the file itself.
func rootImports(root string, slots [][]string, stderr io.Writer) string {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	var lines []string
	for _, slot := range slots {
		for _, name := range slot {
			path := filepath.Join(root, name)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if line, why := instructionImport(path, realRoot, info); why != "" {
				fmt.Fprintf(stderr, "notehelper: the instructions %s are left out: %s\n", path, why)
			} else {
				lines = append(lines, line)
			}
			break
		}
	}
	return strings.Join(lines, "\n")
}

// instructionImport is the import line for the regular file at path, found
// as info, or why there is none: it leads out of realRoot, it changed as it
// was opened, or an import cannot name it ([importable]).
func instructionImport(path, realRoot string, info os.FileInfo) (string, string) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err.Error()
	}
	if !within(realRoot, real) {
		return "", "it leads out of its checkout, to " + real
	}
	file, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err.Error()
	}
	defer func() { _ = file.Close() }()
	if now, err := file.Stat(); err != nil || !now.Mode().IsRegular() || !os.SameFile(now, info) {
		return "", "it changed as it was read"
	}
	if !importable(real) {
		return "", "an import cannot name its path"
	}
	return "@" + strings.ReplaceAll(real, " ", `\ `), ""
}

// importable reports whether an absolute path survives as one Claude Code
// import once its spaces are escaped. Claude Code takes an import from a
// markdown text token, `(?:^|\s)@((?:[^\s\\]|\\ )+)`, cut at `#` (its shipped
// code), so a path holding a backslash, `#`, a control character or a space
// other than U+0020 is no import; nor is one holding what can open an inline
// token mid-path and split the text (a backtick, `*`, `~`, `[`, `]`, `<`, `>`,
// `&`, `!`, or `_` beside anything but a letter or digit). An underscore
// between letters or digits is plain text (measured: `my repo_x`).
func importable(path string) bool {
	runes := []rune(path)
	for i, r := range runes {
		switch {
		case r == ' ':
		case r == '\\' || r == '#' || unicode.IsControl(r) || unicode.IsSpace(r):
			return false
		case strings.ContainsRune("`*~[]<>&!", r):
			return false
		case r == '_':
			if i == 0 || i == len(runes)-1 || !alnum(runes[i-1]) || !alnum(runes[i+1]) {
				return false
			}
		}
	}
	return true
}

// alnum reports whether r is a letter or a digit.
func alnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
