package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path"
	"strings"
)

// THE GROK BUILD HOOK ADAPTER (-adapter grok-hook), a one-shot one: each hook
// in the run's settings runs the helper once per firing, the hook's JSON on
// stdin (measured on Grok Build 1.0.92). PostToolUse and PostToolUseFailure
// take at the bridge's after-a-batch boundary, Stop at the finish, the answer
// on stdout under the firing's event, the token on stderr; with -skill-loads,
// a PostToolUse of read_file that returned a run's skill file (`FileContent`,
// a missing one's `FileNotFound`) reports it loaded first. A step's firings,
// one per call, take what waits, then only what came since (measured). A
// subagent's calls fire them too, under the child's session id, and the main
// session's under the id `-s` or `-r` named (measured on 1.0.92): only a firing
// in the session -thread-file names, the run's own, takes, since a note handed
// to a child binds one delegated piece of work and only the main record confirms.

// grokInputBytes bounds the hook's input read: a PostToolUse carries the call's
// output, whose size is the tool's.
const grokInputBytes = 64 << 20

// grokHookInput is the part of a Grok Build hook's input the adapter reads.
type grokHookInput struct {
	Event     string `json:"hook_event_name"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		TargetFile string `json:"target_file"`
	} `json:"tool_input"`
	ToolResponse struct {
		FileContent json.RawMessage `json:"FileContent"`
	} `json:"tool_response"`
	Cwd     string `json:"cwd"`
	Session string `json:"session_id"`
}

// runGrokHook is one firing: the exit status, the answer on stdout and the take's
// token on stderr. Every failure exits 0 with nothing on stdout, so the harness
// goes on with nothing handed over and the note is told again.
func runGrokHook(ctx context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	_ func(string) string) int {

	logger := log.New(stderr, "notehelper: ", 0)
	raw, err := io.ReadAll(io.LimitReader(stdin, grokInputBytes))
	if err != nil {
		logger.Print("the hook's input could not be read")
		return exitDone
	}
	var in grokHookInput
	if json.Unmarshal(raw, &in) != nil {
		logger.Print("the hook's input is not the JSON object Grok Build hands a hook")
		return exitDone
	}
	cfg, err := readTakeConfig(f.config)
	if err != nil {
		logger.Print(err)
		return exitDone
	}
	ctx, cancel := context.WithTimeout(ctx, onceBudget)
	defer cancel()
	if f.skillLoads && in.Event == "PostToolUse" && in.ToolName == "read_file" && len(in.ToolResponse.FileContent) > 0 {
		if skill := grokSkillRead(cfg.SkillsDir, in.Cwd, in.ToolInput.TargetFile); skill != "" {
			if err = reportLoad(ctx, cfg, skill); err != nil {
				logger.Printf("the skill load was not reported: %v", err)
			}
		}
	}
	if f.loadsOnly {
		return exitDone
	}
	var boundary string
	switch in.Event {
	case "PostToolUse", "PostToolUseFailure":
		boundary = boundaryBatch
	case "Stop":
		boundary = boundaryFinish
	default:
		return exitDone
	}
	if f.threadFile == "" {
		logger.Print("no note was taken: the hook names no -thread-file, the run's own session")
		return exitDone
	}
	main, err := readThread(f.threadFile)
	switch {
	case err != nil:
		logger.Printf("no note was taken: %v", err)
		return exitDone
	case in.Session != main:
		return exitDone
	}
	answer, token, err := take(ctx, cfg, boundary)
	switch {
	case err != nil:
		logger.Printf("no note was taken: %v", err)
		return exitDone
	case answer == "":
		return exitDone
	}
	// THE FIRING'S OWN EVENT: Grok hands a PostToolUseFailure answer on under
	// its own name, and the bridge names one boundary for both.
	if in.Event == "PostToolUseFailure" {
		answer = strings.Replace(answer, `"hookEventName":"PostToolUse"`, `"hookEventName":"PostToolUseFailure"`, 1)
	}
	// THE ANSWER FIRST, then its token: a token on the record stands for an
	// answer written whole.
	if _, err := io.WriteString(stdout, answer); err != nil {
		return exitDone
	}
	_, _ = io.WriteString(stderr, token+"\n")
	return exitDone
}

// grokSkillRead is the listed name of the run's skill whose file file is,
// relative to cwd where it is not absolute, and "" for any other file: a skill
// is `<skills>/crewlet:<name>/SKILL.md`, skills the run's skills directory
// (takeConfig's SkillsDir), so a file of that shape anywhere else, a
// checkout's own `.grok/skills` among them, is no load.
func grokSkillRead(skills, cwd, file string) string {
	if skills == "" || file == "" {
		return ""
	}
	if !path.IsAbs(file) {
		file = path.Join(cwd, file)
	}
	dir, base := path.Split(path.Clean(file))
	if base != "SKILL.md" {
		return ""
	}
	parent, name := path.Split(strings.TrimSuffix(dir, "/"))
	if path.Clean(parent) != path.Clean(skills) || !strings.HasPrefix(name, "crewlet:") {
		return ""
	}
	return name
}
