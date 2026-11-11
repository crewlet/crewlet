package accessdomain_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// delegateVerdict is delegate by's allow, with high confidence, on an operate
// command seat asked for under request.
func delegateVerdict(seat, request, by, command string) accessdomain.NewVerdict {
	return accessdomain.NewVerdict{
		Request: request, Kind: delegated.KindSSHOperate, Rule: 1, Handle: seat,
		Turn: uuid.NewString(), By: by, ByKind: accessdomain.VerdictByAgent,
		Through: accessdomain.VerdictThroughDelegation, Asked: delegated.Allow,
		Confidence: delegated.High, Verdict: delegated.Allow, Final: true,
		Digest: fmt.Sprintf("%064x", len(command)), Host: "api-1", User: "crewlet-ops",
		Subject: command, Reason: "restarts the one service the item names",
		Findings: []string{"no sudo beyond the service unit"},
	}
}

// engineVerdict is the engine's escalation of request at its deadline.
func engineVerdict(seat, request, command string) accessdomain.NewVerdict {
	v := delegateVerdict(seat, request, "", command)
	v.ByKind, v.Asked, v.Confidence = accessdomain.VerdictByEngine, "", ""
	v.Verdict, v.Overruled, v.Reason, v.Findings = delegated.Escalate, []string{"deadline"}, "", nil
	return v
}

// verdictCall is the engine's write of by's verdict on request, under the
// operation derived from the instant its deciding work began.
func verdictCall(minted time.Time, v accessdomain.NewVerdict) accessdomain.Call {
	return accessdomain.Call{By: engineCall().By,
		OpID: accessdomain.VerdictOpID(minted, v.Handle, v.Request, v.By)}
}

// staleRead is the level these cases read at: each node has applied what it
// wrote.
var staleRead = statelog.Freshness{Level: statelog.ReadStale}

// A VERDICT IS SEALED AT REST AND OPENED BY THE READER: what it decided and
// its giver's reason and findings are in no record on the log and no row in
// the estate in the clear; the reader answers every other field as written
// and opens the three under the record they were sealed for, and neither
// text opens moved to another record or swapped for the other.
//
// Mutation: write SubjectSealed as the plain text and the log and the row both
// carry the command; seal the words under the subject's place and the swap
// opens.
func TestAVerdictIsSealedAtRestAndOpenedByTheReader(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	const command = "sudo systemctl restart billing-worker-7731"
	v := delegateVerdict("sre", uuid.NewString(), "reviewer", command)
	v.Reason, v.Findings = "the ledger-sync-4417 unit is the one the item names",
		[]string{"touches only billing-worker-7731", "no shell redirection"}
	v.StdinBytes, v.StdinSHA256, v.Sampled, v.Item = 12, strings.Repeat("ab", 32), true, "OPS-12"
	res, err := node.writer.Verdict(t.Context(), verdictCall(writerClock, v), v)
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("record the verdict: %+v, %v", res, err)
	}
	inClear := []string{"billing-worker-7731", "ledger-sync-4417", "no shell redirection"}
	end, err := log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= end; seq++ {
		_, payload, _, _, err := log.At(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range inClear {
			if bytes.Contains(payload, []byte(text)) {
				t.Fatalf("record %d on the log carries %q in the clear", seq, text)
			}
		}
	}
	for table, rows := range node.dump() {
		for _, row := range rows {
			for _, text := range inClear {
				if strings.Contains(row, text) {
					t.Fatalf("a row of %s holds %q in the clear: %s", table, text, row)
				}
			}
		}
	}
	page, err := node.reader().Verdicts(t.Context(), accessdomain.VerdictQuery{Handle: "sre"}, staleRead)
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("the verdicts answer %+v, %v", page, err)
	}
	row := page.Rows[0]
	want := accessdomain.VerdictRow{Handle: "sre", ID: accessdomain.VerdictID(v.Request, "reviewer"),
		Request: v.Request, Kind: v.Kind, Rule: 1, Turn: v.Turn, By: "reviewer", ByKind: v.ByKind,
		Through: v.Through, Asked: v.Asked, Confidence: v.Confidence, Verdict: v.Verdict, Final: true,
		Overruled: []string{}, Sampled: true, Digest: v.Digest, Host: "api-1", User: "crewlet-ops",
		StdinBytes: 12, StdinSHA256: v.StdinSHA256, Item: "OPS-12", ContentKey: "k1"}
	got := row
	got.SubjectSealed, got.WordsSealed, got.At, got.Position = "", "", time.Time{}, statelog.Position{}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("the verdict's row is\n%+v\nwant\n%+v", got, want)
	}
	subject, reason, findings, err := row.Open(node.sealer)
	if err != nil || subject != command || reason != v.Reason || !slices.Equal(findings, v.Findings) {
		t.Fatalf("the row opens to %q, %q, %q, %v; want what was recorded", subject, reason, findings, err)
	}
	moved := row
	moved.ID = accessdomain.VerdictID(v.Request, "another-reviewer")
	if _, _, _, err := moved.Open(node.sealer); !errors.Is(err, accessdomain.ErrUnreadable) {
		t.Errorf("a verdict moved to another giver's record opens: %v", err)
	}
	swapped := row
	swapped.SubjectSealed, swapped.WordsSealed = row.WordsSealed, row.SubjectSealed
	if _, _, _, err := swapped.Open(node.sealer); !errors.Is(err, accessdomain.ErrUnreadable) {
		t.Errorf("a verdict's two texts swapped open: %v", err)
	}
	if _, _, _, err := row.Open(nil); err == nil {
		t.Error("a verdict opened with no keyring")
	}
}

// ONE GIVER RECORDS ONE VERDICT PER REQUEST: a second verdict by the same giver
// on the same request, under another operation and from either node, is
// recorded already and changes nothing, while a panel's other delegate and the
// engine each record their own; a retry of the same operation is answered as
// its first copy; and two nodes applying the log, and a redelivery of it, hold
// the same rows.
//
// Mutation: key the verdict on its request alone and the panel's second
// delegate is refused; drop ON CONFLICT DO NOTHING from applyVerdict and the
// redelivery fails.
func TestOneGiverRecordsOneVerdictPerRequest(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	a, b := newWriteNode(t, log, "data-1"), newWriteNode(t, log, "data-2")
	request := uuid.NewString()
	first := delegateVerdict("sre", request, "reviewer", "uptime")
	first.Final = false
	call := verdictCall(writerClock, first)
	if res, err := a.writer.Verdict(t.Context(), call, first); err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the first delegate's verdict: %+v, %v", res, err)
	}
	if res, err := a.writer.Verdict(t.Context(), call, first); err != nil || !res.Collapsed {
		t.Errorf("a retry of the first verdict's own operation answers %+v, %v; want its first copy", res, err)
	}
	b.drain()
	again := first
	again.Asked, again.Verdict = delegated.Deny, delegated.Deny
	if _, err := b.writer.Verdict(t.Context(), verdictCall(writerClock.Add(time.Minute), again), again); !errors.Is(err, accessdomain.ErrRecorded) {
		t.Errorf("the same delegate's second verdict on the request answers %v, want ErrRecorded", err)
	}
	second := delegateVerdict("sre", request, "second-reviewer", "uptime")
	if _, err := b.writer.Verdict(t.Context(), verdictCall(writerClock, second), second); err != nil {
		t.Fatalf("the panel's other delegate's verdict: %v", err)
	}
	escalated := engineVerdict("sre", request, "uptime")
	if _, err := a.writer.Verdict(t.Context(), verdictCall(writerClock, escalated), escalated); err != nil {
		t.Fatalf("the engine's verdict: %v", err)
	}
	if _, err := b.writer.Verdict(t.Context(), verdictCall(writerClock.Add(time.Hour), escalated), escalated); !errors.Is(err, accessdomain.ErrRecorded) {
		t.Errorf("the engine's second verdict on the request answers %v, want ErrRecorded", err)
	}
	a.drain()
	b.drain()
	page, err := a.reader().Verdicts(t.Context(), accessdomain.VerdictQuery{Request: request}, staleRead)
	if err != nil {
		t.Fatal(err)
	}
	var givers []string
	for _, row := range page.Rows {
		givers = append(givers, string(row.ByKind)+":"+row.By+":"+string(row.Verdict))
	}
	slices.Sort(givers)
	if want := []string{"agent:reviewer:allow", "agent:second-reviewer:allow", "engine::escalate"}; !slices.Equal(givers, want) {
		t.Errorf("the request's verdicts are %v, want %v", givers, want)
	}
	if !maps.EqualFunc(a.dump(), b.dump(), slices.Equal) {
		t.Fatal("the two nodes' rows differ")
	}
	a.redeliver()
	if !maps.EqualFunc(a.dump(), b.dump(), slices.Equal) {
		t.Fatal("a redelivered log moved a node's rows")
	}
}

// A VERDICT RECORD IS RETAINED BY A BUILD THAT PREDATES IT: the writer stamps
// a verdict, and a verdict's reseal, at the version its kind was introduced
// at, which a build reading only the base format holds back rather than
// faulting on, while a told record beside it stays readable by every build.
//
// Mutation: drop the verdict's row from versionedFields and both records are
// stamped at 1, which a build without the verdict's applier would fault on.
func TestAVerdictRecordIsRetainedByABuildThatPredatesIt(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	if _, err := node.writer.Tell(t.Context(), engineCall(), told(t, "sre", uuid.NewString(), "operate", true)); err != nil {
		t.Fatal(err)
	}
	v := delegateVerdict("sre", uuid.NewString(), "reviewer", "uptime")
	if _, err := node.writer.Verdict(t.Context(), verdictCall(writerClock, v), v); err != nil {
		t.Fatal(err)
	}
	rotated := newWriteNodeWith(t, log, "data-2", rotatedCipher(t))
	rotated.drain()
	if report, err := rotated.writer.Reseal(t.Context(), engineCall().By, rotated.reader()); err != nil || report.Verdicts != 1 {
		t.Fatalf("the reseal moved %+v, %v; want the verdict", report, err)
	}
	end, err := log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stamped := map[accessdomain.OpKind]int{}
	for seq := uint64(1); seq <= end; seq++ {
		_, payload, _, _, err := log.At(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		body, opened := node.verifier.Open(payload)
		if opened != recordseal.Verified {
			t.Fatalf("record %d opens %s", seq, opened)
		}
		env, err := accessdomain.Domain{}.Envelope(body)
		if err != nil {
			t.Fatal(err)
		}
		stamped[accessdomain.OpKind(env.Op)] = env.V
		if env.Kind == string(accessdomain.KindVerdict) && env.ReadableBy(1) {
			t.Errorf("the %s record on %s is readable by a build at the base format", env.Op, env.Subject)
		}
	}
	if want := map[accessdomain.OpKind]int{accessdomain.OpTold: 1, accessdomain.OpVerdict: 2,
		accessdomain.OpResealed: 2}; !maps.Equal(stamped, want) {
		t.Errorf("the records are stamped %v, want %v", stamped, want)
	}
}

// A VERDICT OUTLIVES ITS SEAT'S REMOVAL AND GOES WITH THE RETENTION, as a
// command does: forgetting a removed seat takes what it was told and leaves
// its verdicts, the hour of a verdict past the cutoff is what the duty
// nominates, and an expiry through the cutoff takes the verdict and keeps
// what is newer.
//
// Mutation: delete the seat's verdicts in applyForget and the removal takes
// the audit trail; drop access_verdicts from minAtStatements and the duty
// never nominates the hour.
func TestAVerdictOutlivesItsSeatAndGoesWithTheRetention(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	v := delegateVerdict("sre", uuid.NewString(), "reviewer", "uptime")
	if _, err := node.writer.Verdict(t.Context(), verdictCall(writerClock, v), v); err != nil {
		t.Fatal(err)
	}
	if _, err := node.writer.Issued(t.Context(), engineCall(), certificate("ops", uuid.NewString(), 4)); err != nil {
		t.Fatal(err)
	}
	cutoff := certificateAt(t, node, 4)
	if _, err := node.writer.Forget(t.Context(), engineCall(), "sre"); err != nil {
		t.Fatal(err)
	}
	if got := node.count(`SELECT COUNT(*) FROM access_verdicts WHERE agent_handle = 'sre'`); got != 1 {
		t.Fatalf("the seat's removal left %d verdicts, want its 1", got)
	}
	hours, err := node.reader().ExpirableHours(t.Context(), cutoff, 10)
	if err != nil || len(hours) != 1 {
		t.Fatalf("the duty nominates %v, %v; want the verdict's hour", hours, err)
	}
	if _, err := node.writer.Expire(t.Context(), engineCall(), hours[0], cutoff); err != nil {
		t.Fatalf("expire %s: %v", hours[0], err)
	}
	for statement, want := range map[string]int{
		`SELECT COUNT(*) FROM access_verdicts`:      0,
		`SELECT COUNT(*) FROM access_certificates`:  1,
		`SELECT COUNT(*) FROM access_seat_removals`: 1,
	} {
		if got := node.count(statement); got != want {
			t.Errorf("%s = %d after the retention, want %d", statement, got, want)
		}
	}
	if _, err := node.writer.Expire(t.Context(), engineCall(), hours[0], cutoff); !errors.Is(err, accessdomain.ErrNotDue) {
		t.Errorf("an expiry of an hour with nothing left answers %v, want ErrNotDue", err)
	}
}

// A REKEY RESEALS EVERY VERDICT UNDER THE ACTIVE KEY, as it does a command:
// the census counts the verdict under the key it is sealed with, a node whose
// keyring made a new key active moves both its texts in one record on the
// verdict's own subject, every node applies it, the row opens under the new
// key to the same texts, and a second pass finds nothing to move.
//
// Mutation: count only the commands in SealedKeys and the census says the old
// key seals nothing while a verdict still needs it.
func TestAVerdictIsResealedUnderTheActiveKey(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	old := newWriteNode(t, log, "data-1")
	v := delegateVerdict("sre", uuid.NewString(), "reviewer", "sudo systemctl restart api")
	if _, err := old.writer.Verdict(t.Context(), verdictCall(writerClock, v), v); err != nil {
		t.Fatal(err)
	}
	counts, err := old.reader().SealedKeys(t.Context())
	if err != nil || counts.Values["k1"] != 1 || counts.Outside("k2") != 1 {
		t.Fatalf("the sealed census is %v, %v; want the verdict under k1", counts, err)
	}
	rotated := newWriteNodeWith(t, log, "data-2", rotatedCipher(t))
	rotated.drain()
	report, err := rotated.writer.Reseal(t.Context(), engineCall().By, rotated.reader())
	if err != nil || report.Verdicts != 1 || report.Commands != 0 || report.Unknown != 0 {
		t.Fatalf("the reseal moved %+v, %v; want one verdict", report, err)
	}
	rotated.drain()
	old.drain()
	for _, n := range []*writeNode{old, rotated} {
		if got := n.count(`SELECT COUNT(*) FROM access_verdicts WHERE content_key = 'k2'`); got != 1 {
			t.Fatalf("a node holds %d verdicts under the new key, want 1", got)
		}
	}
	if !maps.EqualFunc(old.dump(), rotated.dump(), slices.Equal) {
		t.Fatal("the two nodes' rows differ after the reseal")
	}
	page, err := rotated.reader().Verdicts(t.Context(), accessdomain.VerdictQuery{}, staleRead)
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("the verdicts answer %+v, %v", page, err)
	}
	subject, reason, findings, err := page.Rows[0].Open(rotated.sealer)
	if err != nil || subject != v.Subject || reason != v.Reason || !slices.Equal(findings, v.Findings) {
		t.Fatalf("the resealed row opens to %q, %q, %q, %v", subject, reason, findings, err)
	}
	again, err := rotated.writer.Reseal(t.Context(), engineCall().By, rotated.reader())
	if err != nil || again.Verdicts != 0 {
		t.Fatalf("a second pass moved %+v, %v; want nothing", again, err)
	}
}

// A VERDICT RECORDS ONLY WHAT ITS GIVER AND ITS PATH ADMIT: the engine's
// overrulings are closed words, never text that could quote the request; an
// answer and a confidence are an agent's alone; the engine names no seat and
// says why it decided; a decision names its ask; an act nobody delegates has
// no verdict; and an operate command names its host. Each is refused before
// anything is published.
//
// Mutation: accept any overruling text and the quoted command is recorded in
// the clear.
func TestAVerdictRecordsOnlyWhatItsGiverAndPathAdmit(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	for name, change := range map[string]func(v *accessdomain.NewVerdict){
		"an overruling in free text": func(v *accessdomain.NewVerdict) {
			v.Overruled = []string{"ran sudo systemctl restart billing"}
		},
		"an engine answer": func(v *accessdomain.NewVerdict) {
			*v = engineVerdict(v.Handle, v.Request, v.Subject)
			v.Asked = delegated.Allow
		},
		"an engine naming a seat": func(v *accessdomain.NewVerdict) {
			*v = engineVerdict(v.Handle, v.Request, v.Subject)
			v.By = "reviewer"
		},
		"an engine saying nothing of why": func(v *accessdomain.NewVerdict) {
			*v = engineVerdict(v.Handle, v.Request, v.Subject)
			v.Overruled = nil
		},
		"a delegate with no confidence": func(v *accessdomain.NewVerdict) { v.Confidence = "" },
		"a delegate no rule routed":     func(v *accessdomain.NewVerdict) { v.Rule = 0 },
		"a decision naming no ask": func(v *accessdomain.NewVerdict) {
			v.Through = accessdomain.VerdictThroughDecision
		},
		"a person outside a decision": func(v *accessdomain.NewVerdict) {
			v.ByKind, v.Asked, v.Confidence, v.Rule = accessdomain.VerdictByPerson, "", "", 0
		},
		"an act never delegated":        func(v *accessdomain.NewVerdict) { v.Kind = delegated.KindOther },
		"an operate command on no host": func(v *accessdomain.NewVerdict) { v.Host = "" },
		"an unknown verdict":            func(v *accessdomain.NewVerdict) { v.Verdict = "approve" },
		"a reason past its bound": func(v *accessdomain.NewVerdict) {
			v.Reason = strings.Repeat("é", accessdomain.VerdictReasonMaxRunes+1)
		},
		"a request that is not one id": func(v *accessdomain.NewVerdict) { v.Request = "call.1" },
		"no digest":                    func(v *accessdomain.NewVerdict) { v.Digest = "" },
	} {
		v := delegateVerdict("sre", uuid.NewString(), "reviewer", "sudo systemctl restart billing")
		change(&v)
		if _, err := node.writer.Verdict(t.Context(), verdictCall(writerClock, v), v); !errors.Is(err, accessdomain.ErrInvalid) {
			t.Errorf("%s answers %v, want ErrInvalid", name, err)
		}
	}
	person := delegateVerdict("sre", uuid.NewString(), "ops-lead", "sudo systemctl restart billing")
	person.ByKind, person.Through, person.Decision = accessdomain.VerdictByPerson, accessdomain.VerdictThroughDecision, uuid.NewString()
	person.Rule, person.Asked, person.Confidence = 0, "", ""
	escalated := engineVerdict("sre", uuid.NewString(), "sudo systemctl restart billing")
	escalated.User = ""
	for name, v := range map[string]accessdomain.NewVerdict{"a person's approval": person,
		"the engine's escalation": escalated} {
		if _, err := node.writer.Verdict(t.Context(), verdictCall(writerClock, v), v); err != nil {
			t.Errorf("%s is refused: %v", name, err)
		}
	}
	if got := node.count(`SELECT COUNT(*) FROM access_verdicts`); got != 2 {
		t.Errorf("the log holds %d verdicts, want the two admitted", got)
	}
}

// THE VERDICTS PAGE NARROWS BY EVERY FILTER IT TAKES, newest first and paged:
// by the requesting seat, the giver, the request, the host, the act, the
// verdict that took effect, the kind of giver, whether it decided its request,
// whether it was sampled, and the broker instant; each answers exactly the
// verdicts it names, a newer peer's verdict on an act this build does not know
// among them, and a page continues where the last one stopped.
//
// Mutation: drop the sampled filter from verdictStatement and the spot-check
// page answers every verdict; drop the act's and the peer's deploy is answered
// as an operate command.
func TestTheVerdictsPageNarrowsByEveryFilter(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	request := uuid.NewString()
	allow := delegateVerdict("sre", request, "reviewer", "uptime")
	allow.Final = false
	deny := delegateVerdict("sre", request, "second-reviewer", "uptime")
	deny.Asked, deny.Verdict, deny.Sampled = delegated.Deny, delegated.Deny, true
	other := delegateVerdict("web", uuid.NewString(), "reviewer", "df -h")
	escalated := engineVerdict("web", uuid.NewString(), "reboot")
	for _, v := range []accessdomain.NewVerdict{allow, deny, other, escalated} {
		if _, err := node.writer.Verdict(t.Context(), verdictCall(writerClock, v), v); err != nil {
			t.Fatalf("record %s's verdict: %v", v.By, err)
		}
	}
	// A NEWER PEER'S VERDICT on an act this build does not know, which it
	// applies all the same: the act is a word on the row, not a shape.
	peer := verdictOf("web", uuid.NewString(), "reviewer")
	peer.Kind, peer.Host, peer.User = "deploy", "", ""
	subject := accessdomain.VerdictSubject("web", peer.Request, "reviewer")
	opID := statelog.NewOpID(time.Now(), "peer")
	body, err := rawRecord(subject, "data-9", opID, accessdomain.OpVerdict, accessdomain.ScopeSet{Subject: true}, peer)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := recordseal.NewSigner(accessdomain.Domain{}.Name(), recordKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := log.Append(t.Context(), subject.Wire(), opID, nil, signer.Seal(body)); err != nil {
		t.Fatalf("append the peer's verdict: %v", err)
	}
	node.drain()
	yes, no := true, false
	r := node.reader()
	all, err := r.Verdicts(t.Context(), accessdomain.VerdictQuery{}, staleRead)
	if err != nil || len(all.Rows) != 5 {
		t.Fatalf("every verdict answers %d rows, %v", len(all.Rows), err)
	}
	// The broker's instant of the third record, which a window starts at.
	third := all.Rows[2].At
	for name, tc := range map[string]struct {
		q    accessdomain.VerdictQuery
		want []string
	}{
		"newest first":   {accessdomain.VerdictQuery{}, []string{"deploy", "deadline", "df -h", "deny", "allow"}},
		"the seat":       {accessdomain.VerdictQuery{Handle: "sre"}, []string{"deny", "allow"}},
		"the giver":      {accessdomain.VerdictQuery{By: "reviewer"}, []string{"deploy", "df -h", "allow"}},
		"the request":    {accessdomain.VerdictQuery{Request: request}, []string{"deny", "allow"}},
		"the act":        {accessdomain.VerdictQuery{Kind: delegated.KindSSHOperate}, []string{"deadline", "df -h", "deny", "allow"}},
		"the verdict":    {accessdomain.VerdictQuery{Verdict: delegated.Deny}, []string{"deny"}},
		"the engine":     {accessdomain.VerdictQuery{ByKind: accessdomain.VerdictByEngine}, []string{"deadline"}},
		"undecided":      {accessdomain.VerdictQuery{Final: &no}, []string{"allow"}},
		"sampled":        {accessdomain.VerdictQuery{Sampled: &yes}, []string{"deny"}},
		"not sampled":    {accessdomain.VerdictQuery{Sampled: &no, Handle: "sre"}, []string{"allow"}},
		"since":          {accessdomain.VerdictQuery{Since: third}, []string{"deploy", "deadline", "df -h"}},
		"until":          {accessdomain.VerdictQuery{Until: third}, []string{"deny", "allow"}},
		"giver and seat": {accessdomain.VerdictQuery{By: "reviewer", Handle: "web"}, []string{"deploy", "df -h"}},
		"the host":       {accessdomain.VerdictQuery{Host: "api-1", Handle: "web"}, []string{"deadline", "df -h"}},
	} {
		page, err := r.Verdicts(t.Context(), tc.q, staleRead)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := labels(page.Rows); !slices.Equal(got, tc.want) {
			t.Errorf("%s answers %v, want %v", name, got, tc.want)
		}
	}
	var paged []string
	q := accessdomain.VerdictQuery{Limit: 2}
	for range 4 {
		page, err := r.Verdicts(t.Context(), q, staleRead)
		if err != nil {
			t.Fatal(err)
		}
		paged = append(paged, labels(page.Rows)...)
		if page.Next == "" {
			break
		}
		q.Cursor = page.Next
	}
	if want := labels(all.Rows); !slices.Equal(paged, want) {
		t.Errorf("pages of two answer %v, want %v", paged, want)
	}
	for name, q := range map[string]accessdomain.VerdictQuery{
		"an act nobody delegates": {Kind: delegated.KindOther},
		"an engine naming seats":  {ByKind: accessdomain.VerdictByEngine, By: "reviewer"},
		"a page too long":         {Limit: accessdomain.MaxPageSize + 1},
	} {
		if _, err := r.Verdicts(t.Context(), q, staleRead); !errors.Is(err, accessdomain.ErrInvalid) {
			t.Errorf("%s answers %v, want ErrInvalid", name, err)
		}
	}
}

// labels names each row by what it decided, or the engine's word for why.
func labels(rows []accessdomain.VerdictRow) []string {
	out := []string{}
	for _, row := range rows {
		switch {
		case row.ByKind == accessdomain.VerdictByEngine:
			out = append(out, row.Overruled[0])
		case row.Kind != delegated.KindSSHOperate:
			out = append(out, string(row.Kind))
		case row.Host == "api-1" && row.Handle == "web":
			out = append(out, "df -h")
		default:
			out = append(out, string(row.Verdict))
		}
	}
	return out
}
