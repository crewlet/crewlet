package executor_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// kimiCheckoutKAP is a `kimi web`, run under node, that records in $HOME/seen
// what Kimi Code 2.7.2 reads as a session starts (agents: the home's
// AGENTS.md, null for none; skills: the home's skills directory's files, a
// link leading nowhere passed over) and
// answers the driver's routes as kimiKAP does. Once the brief is posted it
// copies $HOME/clone-src, where that names a directory, to $HOME/clone-dst,
// as a clone during a tool call would, and sends two of the main agent's tool
// calls ending, a subagent's, and the turn's end; the session goes idle at
// once without a clone, else at the first steer, recording each steer's text
// (steers) and the skills directory then (skills-late). Where $HOME/keep-hands
// is, each steer's turn ends `blocked`, as a UserPromptSubmit hook's block
// ends one, the session's last outcome `failed`.
const kimiCheckoutKAP = `import http from "node:http"
import crypto from "node:crypto"
import fs from "node:fs"
import { join } from "node:path"
const home = process.env.HOME, kimi = process.env.KIMI_CODE_HOME, seen = join(home, "seen")
fs.mkdirSync(seen, { recursive: true })
const record = (name, value) => fs.writeFileSync(join(seen, name), JSON.stringify(value))
const files = (dir) => {
  const out = {}
  if (!fs.existsSync(dir)) return out
  for (const rel of fs.readdirSync(dir, { recursive: true })) if (fs.statSync(join(dir, rel), { throwIfNoEntry: false })?.isFile()) out[rel] = fs.readFileSync(join(dir, rel), "utf8")
  return out
}
const keep = fs.existsSync(join(home, "keep-hands"))
const clone = fs.existsSync(join(home, "clone-src"))
const sid = "session_0123456789abcdef"
let socket = null, brief = "", idle = false, sent = false
const steers = [], turns = [], waits = []
const frame = (text) => {
  const b = Buffer.from(text)
  const head = b.length < 126 ? Buffer.from([0x81, b.length]) : Buffer.from([0x81, 126, b.length >> 8, b.length & 255])
  return Buffer.concat([head, b])
}
const answer = (res, data) => { res.setHeader("content-type", "application/json"); res.end(JSON.stringify({ code: 0, msg: "success", data })) }
const settle = () => { if (!idle) return; for (const res of waits.splice(0)) answer(res, { session: { activity: "idle", last_turn_outcome: keep && steers.length > 0 ? "failed" : "completed" }, pending_interactions: [] }) }
const call = (id) => ({ type: "item.ended", turn_id: "t_1", agent_id: "main", item: { type: "tool_call", item_id: id, name: "Bash", status: "ended", outcome: "completed", input: { command: "git clone" } } })
const flush = () => {
  if (socket === null || brief === "" || sent) return
  sent = true
  if (clone) fs.cpSync(fs.readFileSync(join(home, "clone-src"), "utf8"), fs.readFileSync(join(home, "clone-dst"), "utf8"), { recursive: true })
  const sub = { ...call("call_s"), agent_id: "agent_sub" }
  for (const e of [{ type: "turn.started", turn: { turn_id: "t_1", ordinal: 0, kind: "agent", status: "running" } }, call("call_1"), call("call_2"), sub,
    { type: "item.ended", turn_id: "t_1", agent_id: "main", item: { type: "assistant_message", item_id: "a_1", status: "ended", outcome: "completed", text: "done" } },
    { type: "turn.ended", turn: { turn_id: "t_1", ordinal: 0, kind: "agent", status: "ended", outcome: "completed", steps: [{ index: 1, outcome: "completed", finish_reason: "end_turn", usage: { input_other: 1, output: 1 } }] } }]) socket.write(frame(JSON.stringify(e)))
  idle = !clone
  settle()
}
const server = http.createServer((req, res) => {
  let body = ""
  req.on("data", (c) => (body += c))
  req.on("end", () => {
    if (req.headers.authorization !== "Bearer t") { res.statusCode = 401; return res.end() }
    const p = new URL(req.url, "http://kap").pathname
    if (p === "/api/v1/sessions" && req.method === "POST") {
      record("agents", fs.existsSync(join(kimi, "AGENTS.md")) ? fs.readFileSync(join(kimi, "AGENTS.md"), "utf8") : null)
      record("skills", files(join(kimi, "skills")))
      return answer(res, { id: sid, workspace_id: "wd_kap" })
    }
    if (p === "/api/v1/config") return answer(res, keep ? { hooks: [{ event: "UserPromptSubmit", matcher: "^for ", command: "exit 2" }] } : {})
    if (p === "/api/v1/plugins") return answer(res, { plugins: [] })
    if (p === "/api/v1/sessions/" + sid + "/prompts" && req.method === "POST") {
      brief = JSON.parse(body).prompt_id
      turns.push({ turn_id: "t_1", outcome: "completed", steps: [{ index: 1 }], items: [{ type: "user_message", item_id: brief, text: "go" }] })
      answer(res, { prompt_id: brief, status: "running" })
      return flush()
    }
    if (p === "/api/v1/sessions/" + sid + "/steer") {
      const s = JSON.parse(body)
      steers.push(s.content[0].text)
      record("steers", steers)
      record("skills-late", files(join(kimi, "skills")))
      if (keep) turns.push({ turn_id: "t_" + (turns.length + 1), outcome: "failed", end_reason: "blocked", steps: [], items: [{ type: "user_message", item_id: s.prompt_id, text: s.content[0].text }, { type: "assistant_message", text: "<hook_result hook_event=\"UserPromptSubmit\">\nno hands\n</hook_result>" }] })
      answer(res, { prompt_id: s.prompt_id, status: "running" })
      idle = true
      return settle()
    }
    if (p === "/api/v1/sessions/" + sid + "/prompts") return answer(res, { active: null, queued: [] })
    if (p === "/api/v1/sessions/" + sid + ":wait") { waits.push(res); return settle() }
    if (p === "/api/v1/sessions/" + sid + "/agents/main/turns") return answer(res, { turns, has_more: false })
    if (p === "/api/v1/sessions/" + sid + ":cancel") return answer(res, {})
    if (p === "/api/v1/shutdown") { answer(res, {}); return res.on("finish", () => process.exit(0)) }
    res.statusCode = 404
    res.end()
  })
})
server.on("upgrade", (req, sock) => {
  const accept = crypto.createHash("sha1").update(req.headers["sec-websocket-key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64")
  sock.write("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")
  sock.on("data", () => {})
  sock.on("error", () => {})
  socket = sock
  flush()
})
server.listen(0, "127.0.0.1", () => {
  const origin = "http://127.0.0.1:" + server.address().port
  process.stdout.write(JSON.stringify({ origin, ws_url: origin.replace("http", "ws") + "/api/v1/ws", token: "t" }) + "\n")
})
`

// kimiCheckoutBox is a real box with a Kimi runner, its `kimi` 2.7.2 serving
// [kimiCheckoutKAP] under node.
func kimiCheckoutBox(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "a Kimi run's driver and its stand-in server run under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
	runner, box := standInBox(t, executor.NewKimi(), map[string]string{
		"node": "#!/bin/sh\nexec " + shellword.Quote(node) + " \"$@\"\n",
		"kimi": "#!/bin/sh\ncase \"$1\" in --version) echo 2.7.2; exit 0;; web) exec " + shellword.Quote(node) +
			" \"$HOME/kap.mjs\";; esac\nexit 1\n",
	})
	plant(t, box, map[string]string{"kap.mjs": kimiCheckoutKAP})
	return runner, box
}

// kimiHomeSkills is the home's skills directory's files, by path below it.
func kimiHomeSkills(t *testing.T, p executor.Paths) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(p.KimiSkills(), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			rel, _ := filepath.Rel(p.KimiSkills(), path)
			out[rel] = string(raw)
		}
		return nil
	})
	return out
}

// kimiHomeAgents is the home's AGENTS.md, "" for none.
func kimiHomeAgents(p executor.Paths) string {
	raw, _ := os.ReadFile(p.KimiHomeAgents())
	return string(raw)
}

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A KIMI RUN: of ./repo,
// holding every file Kimi Code 2.7.2 reads where it starts and more, its
// .kimi-code/AGENTS.md and AGENTS.md (not agents.md) are handed under its root
// in KIMI_CODE_HOME/AGENTS.md, after the home's own, else in one the run
// makes, the home's own again after; its skills a walk of .kimi-code/skills
// and .agents/skills reaches come as `repo:<skill>` in the home's skills, none
// past the run; nothing else reaches Kimi, and the start directory's own
// AGENTS.md is left whole. A clone during the run is steered at the main
// loop's next tool call, once, in Kimi's wrapping.
//
// Mutation: drop the gate, the driver's hand or restore's removeCopies, read
// agents.md first, walk flat, or hand twice: red.
func TestWhatACheckoutBelowTheStartDirectoryBringsAKimiRun(t *testing.T) {
	t.Parallel()
	repo := piCheckout("workspace/repo", map[string]string{
		".kimi-code/AGENTS.md": "REPO-DOT\n", "AGENTS.md": "REPO-AGENTS\n", "agents.md": "REPO-LOWER\n",
		"Agents.md": "REPO-MIXED\n", "AGENT.md": "REPO-AGENT\n", "KIMI.md": "REPO-KIMI\n", "CLAUDE.md": "REPO-CLAUDE\n",
		".kimi/AGENTS.md": "REPO-DOT-KIMI\n", ".agents/AGENTS.md": "REPO-DOT-AGENTS\n", "sub/AGENTS.md": "REPO-SUB\n",
		".kimi-code/mcp.json": `{"mcpServers":{"REPO-MCP":{"command":"x"}}}`, ".kimi-code/config.toml": "# REPO-CONFIG\n",
		".kimi-code/agents/a.md":                      "REPO-AGENTFILE\n",
		".kimi-code/skills/check/SKILL.md":            "---\nname: check\ndescription: Checks.\n---\nKIMI-CHECK\n",
		".kimi-code/skills/group/deep/SKILL.md":       "---\nname: deep\ndescription: Deep.\n---\nDeep.\n",
		".kimi-code/skills/check/inner/SKILL.md":      "---\nname: inner\ndescription: Below a skill.\n---\n",
		".kimi-code/skills/node_modules/n/SKILL.md":   "---\nname: n\ndescription: A package's.\n---\n",
		".agents/skills/check/SKILL.md":               "---\nname: check\ndescription: Not this one.\n---\nAGENTS-CHECK\n",
		".agents/skills/lint/SKILL.md":                "---\nname: lint\ndescription: Lints.\n---\nLint.\n",
		".kimi/skills/k/SKILL.md":                     "---\nname: k\ndescription: Kimi's old.\n---\n",
		".claude/skills/c/SKILL.md":                   "---\nname: c\ndescription: Claude's.\n---\n",
		".codex/skills/x/SKILL.md":                    "---\nname: x\ndescription: Codex's.\n---\n",
		".kimi-code/skills/.hidden/h/SKILL.md":        "---\nname: h\ndescription: Hidden.\n---\n",
		".kimi-code/skills/group/deep/notes/SKILL.md": "---\nname: notes\ndescription: Below deep.\n---\n",
	})
	marks := []string{"REPO-LOWER", "REPO-MIXED", "REPO-AGENT\n", "REPO-KIMI", "REPO-CLAUDE", "REPO-DOT-KIMI",
		"REPO-DOT-AGENTS", "REPO-SUB", "REPO-MCP", "REPO-CONFIG", "REPO-AGENTFILE", "AGENTS-CHECK",
		"A package's", "Kimi's old", "Claude's", "Codex's", "Hidden."}
	for _, c := range []struct {
		name string
		own  string
		late bool
	}{
		{name: "a home with no AGENTS.md, and a checkout cloned during the run", late: true},
		{name: "the home's own AGENTS.md", own: "HOME-OWN\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runner, box := kimiCheckoutBox(t)
			p := executor.PathsFor(box)
			plant(t, box, repo)
			plant(t, box, map[string]string{"workspace/AGENTS.md": "W-OWN\n"})
			person := map[string]string{"mine/SKILL.md": "---\nname: mine\ndescription: The person's.\n---\n"}
			for rel, body := range person {
				plantFile(t, filepath.Join(p.KimiSkills(), rel), body)
			}
			if c.own != "" {
				plantFile(t, p.KimiHomeAgents(), c.own)
			}
			if c.late {
				late := t.TempDir()
				for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE-INSTRUCTIONS\n",
					".agents/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\nFix.\n",
					".kimi-code/config.toml":      "# LATE-CONFIG\n"}) {
					plantFile(t, filepath.Join(late, rel), body)
				}
				plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
					"clone-dst": filepath.Join(box.Home(), "workspace", "late")})
			}
			if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
				t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
			}
			work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			// Instructions.
			var agents *string
			seenBy(t, box, "agents", &agents)
			want := "for `repo` with root " + work + "/repo\n\nREPO-DOT\n\n\nREPO-AGENTS\n"
			if c.own != "" {
				want = c.own + "\n\n" + want
			}
			if agents == nil || *agents != want {
				t.Errorf("instructions: Kimi started on the home's AGENTS.md %v, want %q", agents, want)
			}
			if got := boxFile(t, box, box.Home()+"/workspace/AGENTS.md"); got != "W-OWN\n" || strings.Contains(*agents, "W-OWN") {
				t.Errorf("instructions: the start directory's own AGENTS.md reads %q after the run, want it whole and apart", got)
			}
			if got := kimiHomeAgents(p); got != c.own {
				t.Errorf("instructions: after the run the home's AGENTS.md is %q, want the home's own %q", got, c.own)
			}
			// Skills.
			var skills map[string]string
			seenBy(t, box, "skills", &skills)
			// A skill below another is its file, copied whole with it, never a skill of its own.
			if got := slices.Sorted(maps.Keys(skills)); !slices.Equal(got, []string{"mine/SKILL.md", "repo:check/SKILL.md",
				"repo:check/inner/SKILL.md", "repo:deep/SKILL.md", "repo:deep/notes/SKILL.md", "repo:lint/SKILL.md"}) {
				t.Errorf("skills: Kimi started on %q in the home's skills", got)
			}
			if got := skills["repo:check/SKILL.md"]; got != "---\nname: \"repo:check\"\ndescription: Checks.\n---\nKIMI-CHECK\n" {
				t.Errorf("skills: repo:check reads %q, want .kimi-code/skills' own, renamed", got)
			}
			if got := kimiHomeSkills(t, p); !maps.Equal(got, person) {
				t.Errorf("skills: after the run the home's skills hold %q, want the person's alone", got)
			}
			// Everything else.
			raw, _ := json.Marshal([]any{agents, skills})
			for _, mark := range append(marks, "LATE-CONFIG") {
				if strings.Contains(string(raw), mark) {
					t.Errorf("%s: what the checkout should not bring reached what Kimi starts on", mark)
				}
			}
			// A checkout cloned during the run.
			if !c.late {
				return
			}
			var steers []string
			seenBy(t, box, "steers", &steers)
			late := work + "/late"
			wantLate := "for `late` with root " + late + "\n\n<!-- From: " + late + "/AGENTS.md -->\nLATE-INSTRUCTIONS\n\n" +
				"# Skills of " + late + "\n\nEach is a set of local instructions to follow, stored in its `SKILL.md` file; " +
				"to use one, read that file.\n- late:fix: Fixes. (file: " + p.KimiSkills() + "/late:fix/SKILL.md)"
			if !slices.Equal(steers, []string{wantLate}) {
				t.Errorf("clone: the run steered %q, want once\n%q", steers, wantLate)
			}
			var lateSkills map[string]string
			seenBy(t, box, "skills-late", &lateSkills)
			if got := lateSkills["late:fix/SKILL.md"]; got != "---\nname: \"late:fix\"\ndescription: Fixes.\n---\nFix.\n" {
				t.Errorf("clone: its skill read %q while the run held it", got)
			}
			if errs := boxFile(t, box, p.Err()); strings.Contains(errs, "no note helper") || strings.Contains(errs, "crewlet: the note helper") {
				t.Errorf("the run said its checkouts were not handed:\n%s", errs)
			}
		})
	}
}

// WHAT A KIMI RUN'S FIRING LEFT OUT, AND A HAND A HOOK KEPT, REACH ITS ERROR
// STREAM ONCE, AFTER KIMI: a clone's skill whose name a person's skill in
// ~/.agents/skills takes, case aside, is not copied, as the copy would hide
// it, but steered with the line, which the script prints once Kimi stops; a
// hand a UserPromptSubmit hook blocks never ends the run (it carries no note,
// brief or tool of the run's): the run succeeds, the skip log saying once
// which hook kept it.
//
// Mutation: drop the print or Kimi's listing rule, refuse a kept hand, take
// its turn for the run's outcome, or say it per hand: a row goes red.
func TestAKimiRunsLeftOutLinesAndKeptHandsReachItsErrorStreamOnce(t *testing.T) {
	t.Parallel()
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("a hook keeps the hand %v", keep), func(t *testing.T) {
			t.Parallel()
			runner, box := kimiCheckoutBox(t)
			p := executor.PathsFor(box)
			plant(t, box, piCheckout("workspace/repo", map[string]string{"AGENTS.md": "REPO\n"}))
			mine := filepath.Join(box.Home(), ".agents", "skills", "mine")
			plantFile(t, filepath.Join(mine, "SKILL.md"), "---\nname: Late:Fix\ndescription: Mine.\n---\n")
			late := t.TempDir()
			for rel, body := range piCheckout("late", map[string]string{"AGENTS.md": "LATE\n",
				".agents/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\n"}) {
				plantFile(t, filepath.Join(late, rel), body)
			}
			plant(t, box, map[string]string{"clone-src": filepath.Join(late, "late"),
				"clone-dst": filepath.Join(box.Home(), "workspace", "late")})
			if keep {
				plant(t, box, map[string]string{"keep-hands": ""})
			}
			if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
				t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
			}
			work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			line := "the skill " + work + "/late/.agents/skills/fix is left out: the home's " + mine +
				", which Kimi Code lists as Late:Fix, takes the name late:fix it would list the copy under"
			var steers []string
			seenBy(t, box, "steers", &steers)
			if len(steers) != 1 || !strings.Contains(steers[0], "- "+line) {
				t.Errorf("the run steered %q, want the line in its one hand", steers)
			}
			var lateSkills map[string]string
			seenBy(t, box, "skills-late", &lateSkills)
			if _, ok := lateSkills["late:fix/SKILL.md"]; ok {
				t.Errorf("the copy late:fix was made beside the person's own Late:Fix: %q", lateSkills)
			}
			errs := boxFile(t, box, p.Err())
			if n := strings.Count(errs, line); n != 1 {
				t.Errorf("the run's error stream says the line %d times, want once:\n%s", n, errs)
			}
			kept := "crewlet: a UserPromptSubmit hook kept the instructions and skills of a checkout cloned during the run " +
				"from the model: " + p.KimiHome() + `/config.toml: its [[hooks]] entry event UserPromptSubmit, matcher "^for "`
			if n, want := strings.Count(errs, kept), map[bool]int{true: 1}[keep]; n != want {
				t.Errorf("the run's error stream says a hook kept the hand %d times, want %d:\n%s", n, want, errs)
			}
			if strings.Contains(errs, "kept the run's") || strings.Contains(errs, "Kimi kept") {
				t.Errorf("the run was refused for a hand:\n%s", errs)
			}
		})
	}
}

// kimiRestoreStandIn is a `node` that records the home's AGENTS.md as the
// run's driver starts, then does what $HOME/mode says: exit 0, exit 1, signal
// its script as a teardown does (TERM, INT or HUP) and wait to be ended, or
// kill its script outright.
const kimiRestoreStandIn = `#!/bin/sh
if [ -e "$KIMI_CODE_HOME/AGENTS.md" ]; then cat "$KIMI_CODE_HOME/AGENTS.md"; else echo NONE; fi > "$HOME/seen-agents"
case "$(cat "$HOME/mode")" in
ok) exit 0;;
fail) exit 1;;
term|int|hup) kill -"$(tr a-z A-Z < "$HOME/mode")" "$PPID"; exec sleep 60;;
kill) kill -KILL "$PPID"; exit 0;;
esac
exit 2
`

// kimiGateFails is a `mkdir` that fails for the run's checkout state, so the
// gate stops after it has held the home's AGENTS.md.
const kimiGateFails = `#!/bin/sh
for a; do case "$a" in */.crewlet/kimi-checkouts) exit 1;; esac; done
exec /bin/mkdir "$@"
`

// THE HOME'S AGENTS.md AND SKILLS ARE THE HOME'S OWN HOWEVER A KIMI RUN ENDS:
// put back once the driver has stopped, succeeded or failed, where the gate
// fails after holding, and on each teardown signal (TERM, INT, HUP), ending
// the driver first; a run killed outright is put back by the next run's
// clear, whichever harness it drives. The home's own AGENTS.md comes back as
// it was, one the run made is gone, no copy is left, and nothing held is.
//
// Mutation: drop the put-back after the driver, the EXIT trap or a signal's,
// or Kimi's homeKeeper, and a row goes red.
func TestAKimiRunPutsTheHomesAGENTSmdBackHoweverItEnds(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ok", "fail", "gate", "term", "int", "hup", "kill"} {
		for _, own := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, home's own %v", mode, own), func(t *testing.T) {
				t.Parallel()
				runner, box := standInBox(t, executor.NewKimi(), map[string]string{"node": kimiRestoreStandIn,
					"kimi": "#!/bin/sh\necho 2.7.2\n"})
				p := executor.PathsFor(box)
				plant(t, box, piCheckout("workspace/repo", map[string]string{"AGENTS.md": "REPO\n",
					".agents/skills/s/SKILL.md": "---\nname: s\ndescription: S.\n---\n"}))
				plant(t, box, map[string]string{"mode": mode})
				home := ""
				if own {
					home = "OWN\n"
					plantFile(t, p.KimiHomeAgents(), home)
				}
				if mode == "gate" {
					writeBoxStandIn(t, box, p.BinDir()+"/mkdir", kimiGateFails)
				}
				handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "go"})
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				awaitOver(t, runner, box, handle, time.Minute, "the stand-in ends at once or asks its script to")
				work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
				if err != nil {
					t.Fatal(err)
				}
				if mode != "gate" {
					want := "for `repo` with root " + work + "/repo\n\nREPO\n"
					if own {
						want = "OWN\n\n\n" + want
					}
					if got := boxFile(t, box, box.Home()+"/seen-agents"); got != want {
						t.Fatalf("the driver started on the home's AGENTS.md\n%q\nwant\n%q", got, want)
					}
				}
				if mode == "kill" {
					if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
						t.Fatalf("the next run's clear: %v", err)
					}
				}
				if got := kimiHomeAgents(p); got != home {
					t.Errorf("after the run the home's AGENTS.md is %q, want the home's own %q", got, home)
				}
				if got := kimiHomeSkills(t, p); len(got) != 0 {
					t.Errorf("after the run the home's skills hold %q, want no copy", got)
				}
				for _, held := range []string{"", ".none", ".live", ".tmp"} {
					if _, err := os.Lstat(p.KimiHomeInstructions() + held); err == nil {
						t.Errorf("%s is left after the run", p.KimiHomeInstructions()+held)
					}
				}
			})
		}
	}
}
