package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// WHERE A GEMINI CLI OR QWEN CODE BOX COMPRESSES A CONTINUED SESSION, read in
// the box the launch gets (chatcount.go has what is compared with it): the
// threshold of the window its release states for the run's model, unless a
// source the box's harness loads can move either, or what the first request
// masks or clears. Runs have full access, so a box's own settings, `.env`
// files, environment and sign-in are its harness's: each is searched for the
// knobs' names in its raw bytes, never parsed (a comment or a `$VAR` names
// one as surely), and a find is [sandbox.PointSettings], which only tells
// more. Gemini CLI's box also sizes the session context its resume hands over
// ([geminiContextBytes]).

// chatPointWait bounds the read: one shell, a version, a few greps, and for
// Gemini CLI a walk of the workspace's tree. Sixty seconds is a walk of
// millions of entries on a contended disk; past it the point is unread and
// the continuation told as a summary.
const chatPointWait = 60 * time.Second

// geminiThreshold is 0.70.0's compressionThreshold default: it compresses
// where its estimate is at least half the model's window (524,288 of
// 1,048,576, measured to the quarter token).
const geminiThresholdDivisor = 2

// Qwen Code 0.28.4's point (measured): autoCompactThreshold 0.85 of the
// window, held at least the summary's reserve and a buffer (33,000) below it.
const (
	qwenThresholdPercent = 85
	qwenReserve          = 33_000
)

// qwenReminderBytes is the dated reminder 0.28.4 adds to the brief, 174
// characters at most in its en-US form, at a byte a token far above the 44
// tokens it counts.
const qwenReminderBytes = 174

// The bytes Gemini CLI 0.70.0's session context holds beside its tree and
// memory files (measured at 429 to 466 for an empty workspace: the date, the
// platform, the temp and workspace directories), at 2 KiB beside four times
// the working directory's path; the most items its tree lists (200); and the
// bytes of a tree line beside its name, each level's indent at its six UTF-8
// bytes, the connector's twelve and a slash, rounded up.
const (
	geminiContextBase     = 2 << 10
	geminiTreeItems       = 200
	geminiTreeIndentBytes = 6
	geminiTreeLineBytes   = 16
)

// geminiPointKeys are the settings names that move Gemini CLI 0.70.0's point,
// window, masking, collapse or session context (G1, read and measured); a
// `.env` file can set GEMINI_EXP and the system settings' paths.
var (
	geminiPointKeys = []string{"compressionThreshold", "contextManagement", "generalistProfile",
		"powerUserProfile", "stressTestProfile", "dynamicModelConfiguration", "modelConfigs", "GEMINI_EXP",
		"includeDirectoryTree", "fileName", "memoryBoundaryMarkers"}
	geminiEnvKeys = []string{"GEMINI_EXP", "GEMINI_CLI_SYSTEM_SETTINGS_PATH", "GEMINI_CLI_SYSTEM_DEFAULTS_PATH"}
)

// qwenPointKeys are the settings names that move Qwen Code 0.28.4's point,
// window or image compaction (Q4, measured); a box-side source naming
// `"model"` at all may name the model, which overrides the run's pin, so it is
// searched for that too, the run's own settings (which name a model block of
// their own) not.
var (
	qwenPointKeys = []string{"autoCompactThreshold", "contextWindowSize", "chatCompression", "sessionTokenLimit",
		"modelProviders", "QWEN_COMPACT_SCREENSHOT"}
	qwenEnvKeys = []string{"QWEN_COMPACT_SCREENSHOT"}
)

// chatPointSource is one file a box's harness loads, searched for keys.
type chatPointSource struct {
	file string
	keys []string
}

// chatPointRead is what a box's point read asks: its harness's version
// command where the launch has no release, the sources, the `.env` names and
// the chain's file names, the environment names, and whether to size Gemini
// CLI's session context.
type chatPointRead struct {
	version   string
	sources   []chatPointSource
	envFiles  []string
	envKeys   []string
	envNames  []string
	signIn    bool
	context   bool
	geminiDir string
}

// chatPointScript is the one exec the read runs: every line it prints is the
// read's, `release`, `hit`, `cwd`, `tree`, `memory`, `import` and `done`.
func chatPointScript(paths Paths, read chatPointRead) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`PATH=%s:"$PATH"; export PATH`, q(paths.BinDir()))
	if read.version != "" {
		line(`printf 'release %%s\n' "$(%s < /dev/null 2> /dev/null | tr '\n' ' ')"`, read.version)
	}
	line(`echo "cwd $PWD"`)
	grep := func(keys []string) string {
		args := make([]string, len(keys))
		for i, k := range keys {
			args[i] = q(k)
		}
		return strings.Join(args, " ")
	}
	// A SOURCE IS A HIT where it holds any key, raw: `chk <file> <key>...`.
	line(`chk() { f=$1; shift; if [ -n "$f" ] && [ -f "$f" ]; then for k; do ` +
		`if grep -qF -e "$k" "$f" 2> /dev/null; then echo "hit $f"; return 0; fi; done; fi; return 0; }`)
	for _, s := range read.sources {
		line(`chk %s %s`, s.file, grep(s.keys))
	}
	if len(read.envFiles) > 0 {
		var files []string
		for _, f := range read.envFiles {
			files = append(files, `"$w"/`+q(f))
		}
		line(`w=$PWD; while :; do for f in %s; do chk "$f" %s; done; [ "$w" = / ] && break; w=$(dirname "$w"); done`,
			strings.Join(files, " "), grep(read.envKeys))
		for _, f := range read.envFiles {
			line(`chk "$HOME"/%s %s`, q(f), grep(read.envKeys))
		}
	}
	for _, name := range read.envNames {
		line(`if env | grep -q %s; then echo %s; fi`, q("^"+name), q("hit the environment's "+name))
	}
	// ONLY THE KEY SIGN-IN WAS MEASURED (MEASURE-P2 G1): a Google login's
	// server sets the masking thresholds, and Vertex AI's were never read.
	if read.signIn {
		line(`if [ -z "${GEMINI_API_KEY:-}" ]; then ` +
			`echo 'hit a sign-in other than a key, a Google login or Vertex AI, whose masking was not measured'; fi`)
	}
	if read.context {
		line(`LC_ALL=C find . -mindepth 1 2> /dev/null | LC_ALL=C awk -F/ '{ l = %d * (NF - 1) + length($NF) + %d; `+
			`if (l > m) m = l } END { print "tree", m + 0 }' || exit 71`, geminiTreeIndentBytes, geminiTreeLineBytes)
		line(`mem() { if [ -f "$1" ]; then echo "memory $(wc -c < "$1" | tr -d ' ')"; ` +
			`if grep -q @ "$1" 2> /dev/null; then echo import; fi; fi; }`)
		line(`find . -name GEMINI.md -type f 2> /dev/null | while IFS= read -r f; do mem "$f"; done`)
		line(`w=$PWD; while [ "$w" != / ]; do w=$(dirname "$w"); mem "$w/GEMINI.md"; done`)
		line(`mem %s`, q(read.geminiDir+"/GEMINI.md"))
	}
	line(`echo done`)
	return b.String()
}

// chatPointAnswer is what the read's lines said.
type chatPointAnswer struct {
	release string
	hits    []string
	cwd     string
	tree    int64
	memory  int64
	imports bool
	done    bool
}

// readChatPointAnswer reads the read's lines; an error for a line it does not
// read.
func readChatPointAnswer(out string) (chatPointAnswer, error) {
	var a chatPointAnswer
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		key, value, _ := strings.Cut(l, " ")
		switch key {
		case "release":
			a.release = releaseOf(value)
		case "hit":
			a.hits = append(a.hits, value)
		case "cwd":
			a.cwd = value
		case "tree", "memory":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return a, fmt.Errorf("its %s line %q does not read", key, l)
			}
			if key == "tree" {
				a.tree = n
			} else {
				a.memory += n
			}
		case "import":
			a.imports = true
		case "done":
			a.done = true
		default:
			return a, fmt.Errorf("its line %q is none the read writes", l)
		}
	}
	if !a.done {
		return a, fmt.Errorf("it ended before its last line")
	}
	return a, nil
}

// readChatPoint runs read in box for ask and decides the point of harness (c,
// named for a person) at release by point, which turns the answer into the
// point where nothing marked it.
func readChatPoint(ctx context.Context, box sandbox.Sandbox, ask sandbox.PointAsk, c carrier, harness,
	release string, read chatPointRead, point func(chatPointAnswer) sandbox.BoxPoint) (sandbox.BoxPoint, error) {

	if ask.Release != "" {
		if p, marked := chatReleaseMarked(harness, release, ask.Release, ask.Writer); marked {
			return p, nil
		}
	} else {
		read.version = c.VersionCommand()
	}
	res, err := box.Exec(ctx, "sh -lc "+shellword.Quote(chatPointScript(PathsFor(box), read)),
		sandbox.ExecOptions{Env: ask.Env, TimeoutSec: chatPointWait.Seconds()})
	if err != nil {
		return sandbox.BoxPoint{}, fmt.Errorf("executor: reading the box's %s point: %w", harness, err)
	}
	if res.ExitCode != 0 {
		return pointMarked(sandbox.PointUnread, fmt.Sprintf("the read of the box's %s point exited %d", harness,
			res.ExitCode)), nil
	}
	a, err := readChatPointAnswer(res.Stdout)
	if err != nil {
		//nolint:nilerr // An answer that does not read is an unread point, never held: the point is the answer.
		return pointMarked(sandbox.PointUnread, "the read's own lines did not read: "+err.Error()), nil
	}
	boxRelease := ask.Release
	if boxRelease == "" {
		boxRelease = a.release
	}
	if p, marked := chatReleaseMarked(harness, release, boxRelease, ask.Writer); marked {
		return p, nil
	}
	p := point(a)
	p.Release = boxRelease
	if p.Mark == "" && len(a.hits) > 0 {
		return pointMarked(sandbox.PointSettings, fmt.Sprintf("the box can move %s's point, its window or "+
			"what its first request masks or clears: %s", harness, strings.Join(a.hits, "; "))), nil
	}
	return p, nil
}

// chatReleaseMarked is no point where box is not release, the one measured,
// or not writer's.
func chatReleaseMarked(harness, release, box, writer string) (sandbox.BoxPoint, bool) {
	box, writer = strings.TrimSpace(box), strings.TrimSpace(writer)
	switch {
	case box != release:
		return pointMarked(sandbox.PointRelease, fmt.Sprintf("the box runs %s %q, and only %s's point was "+
			"measured", harness, box, release)), true
	case box != writer:
		return pointMarked(sandbox.PointPair, fmt.Sprintf("the box runs %s %s and the session was written by "+
			"%q: no pair of differing releases has been measured", harness, box, writer)), true
	}
	return sandbox.BoxPoint{}, false
}

// readBoxPoint reads, in box, where Gemini CLI compresses a continued
// session's first request ([Runner.ReadBoxPoint]): half the window of the
// launch's model ([geminiPoint]), with the session context the box hands over
// before it as BoxBytes; marked where a source moves it, the context is past
// what the count allows for, or the launch names no model.
func (GeminiCLI) readBoxPoint(ctx context.Context, box sandbox.Sandbox,
	ask sandbox.PointAsk) (sandbox.BoxPoint, error) {

	paths := PathsFor(box)
	settings := func(file string) chatPointSource { return chatPointSource{file: file, keys: geminiPointKeys} }
	read := chatPointRead{
		sources: []chatPointSource{settings(`"$PWD/.gemini/settings.json"`),
			settings(shellword.Quote(paths.GeminiHomeSettings())),
			settings(`/etc/gemini-cli/settings.json`), settings(`/etc/gemini-cli/system-defaults.json`),
			settings(`"${GEMINI_CLI_SYSTEM_SETTINGS_PATH:-}"`), settings(`"${GEMINI_CLI_SYSTEM_DEFAULTS_PATH:-}"`),
			settings(shellword.Quote(paths.GeminiSettings()))},
		envFiles: []string{".gemini/.env", ".env"}, envKeys: geminiEnvKeys, envNames: []string{"GEMINI_EXP"},
		signIn: true, context: true, geminiDir: paths.GeminiDir(),
	}
	return readChatPoint(ctx, box, ask, GeminiCLI{}, "Gemini CLI", geminiRelease, read, func(a chatPointAnswer) sandbox.BoxPoint {
		held := geminiContextBytes(a)
		switch {
		case ask.Model == "":
			return pointMarked(sandbox.PointNoModel, "the launch names no model, and the window Gemini CLI "+
				"picks for none was not measured")
		case a.imports:
			return pointMarked(sandbox.PointBoxSources, "a GEMINI.md file in the box imports another, whose "+
				"size the read does not follow")
		case held > geminiContextAllowance:
			return pointMarked(sandbox.PointBoxSources, fmt.Sprintf("the box's session context may reach %d "+
				"bytes, past the %d the count allows for", held, geminiContextAllowance))
		}
		return sandbox.BoxPoint{Tokens: geminiPoint(ask.Model), BoxBytes: held}
	})
}

// geminiContextBytes is at least the session context a Gemini CLI resume
// hands over in the box a: its fixed part, the working directory four times,
// a tree of its most items at the longest line any entry makes, and every
// GEMINI.md it may load.
func geminiContextBytes(a chatPointAnswer) int64 {
	return geminiContextBase + 4*int64(len(a.cwd)) + geminiTreeItems*a.tree + a.memory
}

// geminiPoint is where 0.70.0 compresses a run on model: half its window.
func geminiPoint(model string) int64 { return geminiWindow(model) / geminiThresholdDivisor }

// readBoxPoint reads, in box, where Qwen Code compresses a continued session's
// first request ([Runner.ReadBoxPoint]): its threshold of the window of the
// launch's model ([qwenPoint]), with the dated reminder beside the brief as
// BoxBytes; marked where a source moves it, or where the run signs in by its
// paid login ([qwenSignIn]), which was never measured: its model list may set
// the model and the window (MEASURE-P2 Q3's first rule).
func (QwenCode) readBoxPoint(ctx context.Context, box sandbox.Sandbox,
	ask sandbox.PointAsk) (sandbox.BoxPoint, error) {

	paths := PathsFor(box)
	boxSide := append(append([]string(nil), qwenPointKeys...), `"model"`)
	envKeys := qwenEnvKeys
	if ask.Model == "" {
		envKeys = append(append([]string(nil), envKeys...), "OPENAI_MODEL")
	}
	read := chatPointRead{
		sources: []chatPointSource{{file: `"$PWD/.qwen/settings.json"`, keys: boxSide},
			{file: shellword.Quote(paths.QwenDir() + "/settings.json"), keys: boxSide},
			{file: `/etc/qwen-code/settings.json`, keys: boxSide},
			{file: `/etc/qwen-code/system-defaults.json`, keys: boxSide},
			{file: `"${QWEN_CODE_SYSTEM_SETTINGS_PATH:-}"`, keys: boxSide},
			{file: `"${QWEN_CODE_SYSTEM_DEFAULTS_PATH:-}"`, keys: boxSide},
			{file: shellword.Quote(paths.QwenSettings()), keys: qwenPointKeys}},
		envFiles: []string{".qwen/.env", ".env"}, envKeys: envKeys,
		envNames: []string{"QWEN_COMPACT_SCREENSHOT"},
	}
	return readChatPoint(ctx, box, ask, QwenCode{}, "Qwen Code", qwenRelease, read, func(chatPointAnswer) sandbox.BoxPoint {
		if qwenSignIn(ask.Env) != qwenAuthOpenAI {
			return pointMarked(sandbox.PointSettings, "the run signs Qwen Code in by its paid login, whose "+
				"model list may set the model and its window, and which was not measured")
		}
		return sandbox.BoxPoint{Tokens: qwenPoint(ask.Model), BoxBytes: qwenReminderBytes}
	})
}

// qwenPoint is where 0.28.4 compresses a run on model: 85% of its window,
// rounded up, at most the window less [qwenReserve] where the window is larger.
func qwenPoint(model string) int64 {
	w := qwenWindow(model)
	point := (w*qwenThresholdPercent + 99) / 100
	if w > qwenReserve {
		point = min(point, w-qwenReserve)
	}
	return point
}
