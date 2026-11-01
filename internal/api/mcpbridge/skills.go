package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// A RUN'S OWN SKILLS, AND WHAT LOADING ONE UNLOCKS.
//
// A seat's executor can be handed the company's tool skills as its harness's
// OWN (internal/sandbox, RunRequest.Skills): listed to its model, and loaded
// with the harness's own skill tool, which tells nobody. So a hook of the
// harness's (Claude Code) or a plugin of its own (OpenCode) reports each load
// here, by the name the skill is listed under, and the bridge records it as
// THE RUN'S OWN LOAD of the Tool Skill that name stands for ([Session.Skills]):
// a call of load_tool_skill with the skill's key, made on the opening as a
// call of the run's is ([binding.Call]). One kind of load, then, however it was
// made: the required-skill guard unlocks what the skill covers by its own
// reading of the call, the skill's page records the read, the run's record
// lists the load, and every rebuild of the run's surface replays it from that
// record, as it replays a load the run made over the bridge.
//
// load_tool_skill IS NEVER LISTED to a harness that loads its skills itself:
// its own tool is the loader its model knows, and a second beside it would
// put the same text in front of the model a second way. It stays on the run's
// surface — a reported load is a call of it.
//
// TWO WAYS IN, each behind the run's own token and nothing else, as a
// person's notes are (steer.go):
//
//   - THE skill_loaded TOOL ([SkillTool]) on the engine's own server, which
//     a hook calls. NEVER LISTED, so no model is offered it, and no model
//     can unlock a skill it never read; answered by the server's receiving
//     middleware, so the hook's call never reaches the run's surface itself;
//     and its answer is ALWAYS empty text, never an error — Claude Code
//     answers a hook's failed call by re-initialising the whole bridge.
//   - THE SKILL ROUTE ([SkillPattern]), which a plugin posts each load to:
//     POST `{"skill": "<name>"}`, answered 204 — and 404 for a session
//     handed no skills, which has no skill route at all.
//
// A NAME THE RUN WAS NOT HANDED — a skill of the checkout's, or of the
// image's — is no Tool Skill of the run's, and is answered as quietly and
// recorded nowhere.

// SkillTool is the tool a harness's hook calls on the bridge once its own skill
// tool has loaded a skill.
const SkillTool = "skill_loaded"

// SkillPattern is the skill route's mux pattern, under the bridge's own
// prefix, so every gate that covers the bridge covers it by prefix, as it
// covers the notes route ([SteerPattern]).
const SkillPattern = PathPrefix + "{token}/skill"

// skillSuffix joins a job's endpoint to its skill route.
const skillSuffix = "/skill"

// SkillURL is the skill route of the job whose endpoint is given — what the
// engine hands a run's harness beside the endpoint itself.
func SkillURL(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	return endpoint + skillSuffix
}

// skillReportBody bounds a report's body: one skill's name, under a hundred
// bytes, and 4 KiB is far past that while far inside what the relay carries.
const skillReportBody = 4 << 10

// skillLoad is what a report names: the skill as the harness listed it, and as
// its model loaded it — `${tool_input.skill}` of Claude Code's hook, the `id`
// OpenCode's skill tool took.
type skillLoad struct {
	Skill string `json:"skill"`
}

// skillKey is the Tool Skill the run's harness listed as name, if it is one the
// run was handed. A leading slash is the harness's command syntax, never part
// of a name.
func (s *Session) skillKey(name string) (string, bool) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	key, ok := s.Skills[name]
	return key, ok && key != ""
}

// nativeSkills reports whether the session's harness loads skills of its own,
// so the bridge's loader is never listed to it.
func (s *Session) nativeSkills() bool { return len(s.Skills) > 0 }

// skillMiddleware answers skill_loaded on the engine's own server, before the
// SDK looks for a tool it never lists.
func (o *binding) skillMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		call, ok := req.(*mcp.CallToolRequest)
		if method != "tools/call" || !ok || call.Params == nil || call.Params.Name != SkillTool {
			return next(ctx, method, req)
		}
		var load skillLoad
		if len(call.Params.Arguments) > 0 {
			// UNREADABLE IS NOTHING TO RECORD: the hook's input is the
			// runner's own shape, and an answer other than empty text would
			// cost the run its bridge.
			if err := json.Unmarshal(call.Params.Arguments, &load); err != nil {
				load = skillLoad{}
			}
		}
		o.loadSkill(load.Skill)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: ""}}}, nil
	}
}

// loadSkill records the run's load of the skill its harness listed as name, as
// a call of load_tool_skill with the skill's key on the opening — or nothing,
// for a name that is no skill of the run's. The call's answer, the skill's
// text, is the harness's already: the model read it from the harness's own
// tool.
func (o *binding) loadSkill(name string) {
	key, ok := o.session.skillKey(name)
	if !ok {
		if strings.HasPrefix(name, "${") {
			// THE HARNESS DID NOT FILL IT IN: which skill was loaded cannot
			// be told, and the run's calls to a tool it covers will name
			// the skill to load again. Said once per opening.
			o.skillUnsubstituted.Do(func() {
				log.WarnContext(o.ctx, "mcp_bridge_skill_unsubstituted", "run_id", o.session.RunID,
					"seat", o.session.Handle, "skill", name,
					"detail", "the run's harness did not fill in the name of the skill its hook "+
						"reported, so no load it makes is recorded; a required skill holds its "+
						"tools back for the whole run")
			})
		}
		return
	}
	o.Call(toolsession.Call{Name: toolname.LoadToolSkill, Arguments: map[string]any{"key": key}})
}

// SkillHandler serves the skill route ([SkillPattern]): the same routing as
// [Bridge.Handler] — served here by the node holding the job's opening, and
// relayed to it by any other.
func (b *Bridge) SkillHandler() http.Handler { return b.routed(b.serveSkill) }

// serveSkill answers one report of the skill route on the opening that holds
// it — admitted, as every route under the prefix is, by the routing in front
// of it ([Bridge.routed]).
func (b *Bridge) serveSkill(w http.ResponseWriter, r *http.Request, opened *binding) {
	if !opened.session.nativeSkills() {
		// A SESSION HANDED NO SKILLS has no skill route at all.
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpjson.Fail(w, http.StatusMethodNotAllowed, httpjson.CodeMethodNotAllowed)
		return
	}
	var load skillLoad
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, skillReportBody))
	if err := dec.Decode(&load); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpjson.Fail(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge)
			return
		}
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	opened.loadSkill(load.Skill)
	w.WriteHeader(http.StatusNoContent)
}

// offeredTool reports whether the opening lists name to the run's harness: every
// tool its surface offers, but the bridge's own skill loader to a harness that
// loads its skills itself ([Session.Skills]).
func (o *binding) offeredTool(name string) bool {
	return name != toolname.LoadToolSkill || !o.session.nativeSkills()
}
