package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME, A REPOSITORY OR A MACHINE'S POLICY CANNOT SWITCH A CLAUDE CODE RUN'S
// HOOKS OFF UNSEEN, AND THE RUN NEVER SWITCHES THEM BACK ON. Claude Code
// 2.1.330 runs no hook at all where the settings it merges leave
// `disableAllHooks` true, a later file's value winning, and only managed hooks
// where a managed file sets `allowManagedHooksOnly`; the run's notes and skill
// loads then go unread with no error (measured). A `false` in the run's own
// settings would win, and run the home's and the repository's hooks where a
// person's session runs none. So a run that starts a hook of its own is
// refused before the harness starts, naming the file and the key
// ([claudeHooksOff], [claudeHooksOffGate]).

// claudeHooksOffWait bounds the one exec reading the settings files: a few
// small files, read in a box that must answer as fast to start the run.
const claudeHooksOffWait = 30 * time.Second

// claudeHooksOffMark opens each file in the answer of [claudeHooksOffCommand],
// followed by the file's size and path.
const claudeHooksOffMark = "crewlet-settings "

// claudeHooksOffFiles is every settings file Claude Code 2.1.330 merges for a
// run, lowest first: the home's (the run's pinned configuration directory),
// the start directory's shared and local files, then the managed ones. It
// reads no `settings.local.json` in the home (measured), and the run's own
// `--settings`, between the local and the managed files, sets neither key.
func claudeHooksOffFiles(paths Paths, start string) []string {
	return append([]string{paths.ClaudeConfigDir() + "/settings.json", start + "/.claude/settings.json",
		start + "/.claude/settings.local.json"}, claudeManagedSettings...)
}

// claudeHooksOffCommand is the shell printing, for each of files the box's
// user can read as a regular file, the mark, its size and its path on a line,
// then its bytes. A file that user cannot read Claude Code skips as missing
// (measured: a directory, mode 000 and a broken link alike), so it is not
// printed. A glob that matches nothing stays a word no file is named.
func claudeHooksOffCommand(files []string) string {
	words := make([]string, len(files))
	for i, f := range files {
		if strings.Contains(f, "*") {
			words[i] = f
			continue
		}
		words[i] = shellword.Quote(f)
	}
	return `for f in ` + strings.Join(words, " ") + `; do [ -f "$f" ] && [ -r "$f" ] || continue; ` +
		`n=$(wc -c < "$f") || continue; printf '` + claudeHooksOffMark + `%s %s\n' $n "$f"; cat -- "$f"; done; exit 0`
}

// claudeSettingsRead is one file of a [claudeHooksOffCommand] answer.
type claudeSettingsRead struct {
	path string
	raw  []byte
}

// claudeHooksOffParse parses out, the answer of [claudeHooksOffCommand], into
// its files in order. A section cut short ends the answer there.
func claudeHooksOffParse(out string) []claudeSettingsRead {
	var files []claudeSettingsRead
	for {
		at := strings.Index(out, claudeHooksOffMark)
		if at < 0 {
			return files
		}
		out = out[at+len(claudeHooksOffMark):]
		head, rest, ok := strings.Cut(out, "\n")
		if !ok {
			return files
		}
		size, name, ok := strings.Cut(head, " ")
		n, err := strconv.Atoi(size)
		if !ok || err != nil || n < 0 || n > len(rest) {
			return files
		}
		files = append(files, claudeSettingsRead{path: name, raw: []byte(rest[:n])})
		out = rest[n:]
	}
}

// claudeHooksOffVerdict is why Claude Code would run none of the run's hooks
// given files, read in merge order: the last file holding `disableAllHooks` as
// a boolean decides, and so does the last managed one holding
// `allowManagedHooksOnly`; "" where neither is true. A file that is no JSON
// object, or holds either key as anything but a boolean, Claude Code ignores
// whole (measured: a string, a comment, a trailing comma), so it decides
// nothing. One it drops for another key's invalid value is still read here,
// a wider refusal than the harness's own, as no schema of it is kept.
func claudeHooksOffVerdict(files []claudeSettingsRead) string {
	var off, managedOnly string
	for _, f := range files {
		var settings map[string]json.RawMessage
		if json.Unmarshal(f.raw, &settings) != nil {
			continue
		}
		all, allSet := claudeBool(settings, "disableAllHooks")
		only, onlySet := claudeBool(settings, "allowManagedHooksOnly")
		if !allSet || !onlySet {
			continue
		}
		if all != nil {
			off = ""
			if *all {
				off = f.path
			}
		}
		if only != nil && claudeManagedFile(f.path) {
			managedOnly = ""
			if *only {
				managedOnly = f.path
			}
		}
	}
	const after = ", and no setting of the run's may switch hooks back on, since that would also run the " +
		"home's and the repository's hooks where a person's session runs none"
	switch {
	case off != "":
		return "Claude Code would run none of the run's hooks, which its notes and skill loads go through: " +
			off + " sets disableAllHooks to true" + after + ". Remove that key from " + off + ", or run without " +
			"notes and skills"
	case managedOnly != "":
		return "Claude Code would run none of the run's hooks, which its notes and skill loads go through: " +
			managedOnly + " sets allowManagedHooksOnly to true, which runs the machine's managed hooks alone" +
			after + ". Remove that key from " + managedOnly + ", or run without notes and skills"
	}
	return ""
}

// claudeBool is settings' key as a boolean: nil where the key is missing or
// null, and false as its second answer where it holds anything else.
func claudeBool(settings map[string]json.RawMessage, key string) (*bool, bool) {
	raw, ok := settings[key]
	if !ok || string(raw) == "null" {
		return nil, true
	}
	var v bool
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	return &v, true
}

// claudeManagedFile reports whether file is one of [claudeManagedSettings].
func claudeManagedFile(file string) bool {
	for _, pattern := range claudeManagedSettings {
		if ok, _ := path.Match(pattern, file); ok {
			return true
		}
	}
	return false
}

// claudeHooksOff is why Claude Code in box would run none of a run's hooks
// ([claudeHooksOffVerdict]), read from the box as its user reads them; ""
// where nothing switches them off.
func claudeHooksOff(ctx context.Context, box sandbox.Sandbox, paths Paths) (string, error) {
	start := path.Join(box.Home(), sandbox.WorkspaceSubdir)
	res, err := box.Exec(ctx, claudeHooksOffCommand(claudeHooksOffFiles(paths, start)),
		sandbox.ExecOptions{TimeoutSec: claudeHooksOffWait.Seconds()})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return "", fmt.Errorf("executor: reading the Claude Code settings that can switch the run's hooks off: %w", err)
	}
	return claudeHooksOffVerdict(claudeHooksOffParse(res.Stdout)), nil
}

// claudeHooksOffGate is the part of a run's script that, once its gates have
// started a hook of the engine's (hk), refuses the run where its settings
// switch hooks off ([Paths.ClaudeHooksOff], [claudeHooksOff]), saying why. A
// run whose gates start none, on a release older than every hook's floor, is
// not refused for it.
func claudeHooksOffGate(paths Paths) string {
	f := shellword.Quote(paths.ClaudeHooksOff())
	return `if [ -n "$hk" ] && [ -s ` + f + ` ]; then printf 'crewlet: %s. The run was refused before Claude Code ` +
		`started.\n' "$(cat ` + f + `)" >&2; exit 1; fi` + "\n"
}
