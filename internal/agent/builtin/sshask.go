package builtin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// Approve once at the decision (ADR-0125): ask_to_operate puts one exact
// operate command to a person as a decision on a work item, composed by the
// engine, whose ask carries its act; once the person chooses the act's
// approving option, the seat's ssh_operate call that matches it byte for byte
// runs without a second prompt.
//
// THE ACT BINDS THE CALL, never the words: it names the server and the digest
// over the server as shown, the command and the stdin, set by the engine from
// the call the tool was given. A model's own words that quote a command bind
// nothing, since what a person read in prose is not a call. Choosing the
// approving option is decided as an SSH approval at its step-up (the tracker
// refuses it otherwise), and the person is held to it again at the hold.

// SSHAskOperateTool is the ask, named after its authority row.
const SSHAskOperateTool = toolname.AskToOperate

// The options of the decision ask_to_operate composes.
const (
	// sshRunOnce is the option whose choice approves the call once.
	sshRunOnce = "run-once"
	// sshNotNow approves nothing.
	sshNotNow = "not-now"
)

// operateCallDigest is the SHA-256, in hex, of what a person approves: the
// server as shown (its binding: address, port, operate user and keys), the
// command exactly and the stdin's digest.
func operateCallDigest(h config.SSHHost, command string, stdin []byte) string {
	sum := sha256.Sum256([]byte("crewlet.ssh.operate.decision\x00" + h.ID + "\x00" +
		sshint.OperateBinding(h) + "\x00" + command + "\x00" + stdinDigest(stdin)))
	return hex.EncodeToString(sum[:])
}

// approvesCall reports whether act is a person's choice of the approving
// option of an ssh_operate act for exactly this call.
func approvesCall(act *sandbox.HeldAct, h config.SSHHost, command string, stdin []byte) bool {
	if act == nil || act.Act == nil {
		return false
	}
	a := act.Act
	return a.Kind == delegated.KindSSHOperate && act.ChoiceID != "" && act.ChoiceID == a.Approve &&
		a.Host == h.ID &&
		subtle.ConstantTimeCompare([]byte(a.Digest), []byte(operateCallDigest(h, command, stdin))) == 1
}

// askToOperate is the ask. It posts through comment_on_work_item, so the
// item's gates and the ask's routing are that tool's, unchanged.
type askToOperate struct {
	deps    SSHDeps
	comment *commentOnWorkItem
}

var _ tools.SeatCallable = (*askToOperate)(nil)

func (t *askToOperate) Name() string { return SSHAskOperateTool }

func (t *askToOperate) Description() string {
	return "Ask a person, on the work item that needs it, to approve one exact operate command before " +
		"you run it: the engine quotes the server, the command and the stdin's SHA-256 in the question. " +
		"Once they choose Run it once, your " + SSHOperateTool + " call with exactly the same host, " +
		"command and stdin runs without a second prompt, once, in the turn their answer starts; any " +
		"difference is asked again. Ask an administrator or a person the company names as an SSH approver."
}

func (t *askToOperate) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{"type": "string", "description": "The work item the command is for. " + itemRef},
			"ask":  map[string]any{"type": "string", "description": "The handle of the person to ask."},
			"host": map[string]any{"type": "string",
				"description": "The server's id: one of the servers your SSH access names."},
			"command": map[string]any{"type": "string", "description": "The command exactly as you will run it."},
			"stdin": map[string]any{"type": "string",
				"description": "The stdin exactly as you will send it, if any. At most 128 KiB."},
			"reason": map[string]any{"type": "string",
				"description": "Why the command is needed, in one line of up to 500 characters."},
		},
		"required":             []string{"item", "ask", "host", "command", "reason"},
		"additionalProperties": false,
	}
}

func (t *askToOperate) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *askToOperate) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if turn == nil || turn.Seat == nil || turn.Org == nil {
		return notInATurn(t.Name()), nil
	}
	handle, refusal := seatTurn(t.Name(), turn)
	if refusal != nil {
		return *refusal, nil
	}
	a, refusal := sshArgsOf(t.Name(), map[string]any{"host": args["host"], "command": args["command"],
		"stdin": args["stdin"], "reason": args["reason"]}, true)
	if refusal != nil {
		return *refusal, nil
	}
	h, refusal := reachedHost(t.Name(), t.deps.Servers.Reach(handle), a.host, config.SSHLevelOperate)
	if refusal != nil {
		return *refusal, nil
	}
	person := strings.TrimSpace(argString(args, "ask"))
	if asked := turn.Org.Role(person); asked == nil || asked.Kind != org.KindHuman {
		return failed(t.Name() + ": `ask` is a person's handle: only a person approves a command."), nil
	}
	question := fmt.Sprintf("May %s run a command on %s as %s?", handle, h.ID, h.Users.Operate)
	var body strings.Builder
	fmt.Fprintf(&body, "%s asks to run this command on the server `%s` (%s, port %d) as `%s`, once.\n\n"+
		"**Command**\n\n%s", handle, h.ID, h.Address, h.PortOrDefault(), h.Users.Operate, codeBlock(a.command))
	if len(a.stdin) > 0 {
		fmt.Fprintf(&body, "\n\n**Standard input**: %d bytes, SHA-256 `%s`\n\n%s", len(a.stdin),
			stdinDigest(a.stdin), codeBlock(string(clipRune(a.stdin, sshStdinPreviewBytes))))
	}
	body.WriteString("\n\n**Reason given**\n\n" + inertMarkdown(redact.Secrets(a.reason)))
	posted := map[string]any{
		"item": argString(args, "item"), "body": body.String(), "ask": person, "asks": true,
		"decision": map[string]any{
			"question": question, "role": string(decision.RoleApprover),
			"recommended": sshRunOnce, "rationale": textcutRunes(a.reason, decision.MaxRationale),
			"options": []any{
				map[string]any{"id": sshRunOnce, "label": "Run it once",
					"detail": fmt.Sprintf("Runs the command quoted above once on %s as %s, exactly as quoted, "+
						"and nothing else.", h.ID, h.Users.Operate)},
				map[string]any{"id": sshNotNow, "label": "Not now",
					"detail": "Nothing runs, and the agent is told."},
			},
		},
	}
	// THE ENGINE'S OWN ACT, the one path an act reaches an ask.
	act := delegated.Act{Kind: delegated.KindSSHOperate, Approve: sshRunOnce, Host: h.ID,
		Digest: operateCallDigest(h, a.command, a.stdin)}
	res, _, err := tools.Invoke(delegated.WithAct(ctx, act), tools.Invocation{
		Name: CommentOnWorkTool, Tool: t.comment, Args: tools.ArgsOf(t.comment, false, posted),
		AsSeat: true, Turn: turn, Where: t.Name(),
	})
	return res, err
}

// textcutRunes is s cut to at most n bytes at a character's start.
func textcutRunes(s string, n int) string { return string(clipRune([]byte(s), n)) }

// RunDecided runs the call now where the decision the turn's answer carried
// out approved exactly it, as the person who chose its approving option: once
// per decision, since the verdict that records it is the decision's first.
// False for a call that is held as every other is.
func (t *sshOperate) RunDecided(ctx context.Context, turn *turnctx.Turn, args map[string]any,
	act *sandbox.HeldAct) (tools.Result, bool) {

	if act == nil || act.Act == nil || act.Act.Kind != delegated.KindSSHOperate {
		return tools.Result{}, false
	}
	handle := turn.Handle()
	a, refusal := sshArgsOf(t.Name(), args, true)
	if refusal != nil {
		return tools.Result{}, false
	}
	h, ok := t.deps.Servers.Reach(handle).Host(a.host)
	if !ok || !approvesCall(act, h, a.command, a.stdin) {
		return tools.Result{}, false
	}
	switch may, err := t.deps.Servers.MayApprove(ctx, act.AnsweredBy); {
	case err != nil:
		log.WarnContext(ctx, "ssh_decision_approver_unread", "seat", handle, "approver", act.AnsweredBy,
			"error", err.Error(), "detail", "the call is held for approval instead")
		return tools.Result{}, false
	case !may:
		return tools.Result{}, false
	}
	// ONE ID PER DECISION, so a second call through it finds the first's
	// verdict and is held for a person like any other.
	held := uuid.NewSHA1(uuid.NameSpaceOID, []byte("crewlet.ssh.decision/"+act.Ask)).String()
	first, err := t.deps.Servers.RecordVerdict(ctx, DelegatedVerdict{
		At: act.AnsweredAt, Request: held, Kind: string(delegated.KindSSHOperate), Handle: handle, Turn: turn.RunID,
		Host: h.ID, User: h.Users.Operate, By: act.AnsweredBy, ByKind: VerdictByPerson,
		Through: VerdictThroughDecision, Decision: act.Ask, Item: act.Item,
		Verdict: string(delegated.Allow), Final: true, Digest: operateCallDigest(h, a.command, a.stdin),
		Subject: a.command, Stdin: a.stdin,
	})
	if err != nil || !first {
		// UNRECORDED, OR NOT THE DECISION'S FIRST USE: the call is held for a
		// person like any other, so an approval never runs twice.
		if err != nil {
			log.WarnContext(ctx, "ssh_decision_verdict_unrecorded", "seat", handle, "error", err.Error())
		}
		return tools.Result{}, false
	}
	ctx = tools.WithApproval(ctx, tools.Approval{By: act.AnsweredBy, HeldID: held,
		Binding: sshint.OperateBinding(h)})
	res, err := t.CallForTurn(ctx, turn, args)
	if err != nil {
		return faulted(ctx, t.Name(), err, t.Name()+" was approved through the decision and did not finish"), true
	}
	return res, true
}

// stdinDigest is the SHA-256 of a call's stdin, in hex, "" for none.
func stdinDigest(stdin []byte) string {
	if len(stdin) == 0 {
		return ""
	}
	sum := sha256.Sum256(stdin)
	return hex.EncodeToString(sum[:])
}
