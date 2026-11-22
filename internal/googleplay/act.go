package googleplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// step is one vendor request of an act, the file it uploads, and its
// target's state as read before it is made.
type step struct {
	req    googlegrowth.Request
	media  *File
	before []byte
	class  growth.Class
}

// act is a write as the engine makes it: one request, or the changes staged
// in one edit and the commit that publishes them.
type act struct {
	app    string
	steps  []step
	edit   bool
	commit url.Values
	class  growth.Class
	reason string
	// summary says what a curated act does, for a person; "" for a generic
	// one, whose steps say it.
	summary string
	// halting marks a halt, whose track is written from the track as read
	// (shapeHalt), of the release halt names or the one rolling out.
	halting bool
	halt    []string
}

// actOf builds call's act, holds it to the seat's apps, reads each target's
// state (inside a throwaway edit for staged changes) and classes it.
func (p *Product) actOf(ctx context.Context, call growth.Call) (act, error) {
	a, err := p.build(ctx, call)
	if err != nil {
		return a, err
	}
	for _, s := range a.steps {
		if err = p.inScope(call.Handle, s.req); err != nil {
			return a, err
		}
		if s.req.Account != a.app && a.edit {
			return a, refusal(growth.RefusedInvalid, "every change of one edit is to the edit's own app. No vendor "+
				"was reached.")
		}
	}
	read := func(editID string) error { return p.readBefores(ctx, call.Handle, &a, editID) }
	if a.edit {
		err = p.inThrowawayEdit(ctx, call.Handle, a.app, read)
	} else {
		err = read("")
	}
	if err != nil {
		return a, err
	}
	if err := shapeHalt(&a); err != nil {
		return a, err
	}
	a.class = p.classOfAct(a)
	return a, nil
}

// build is call's act before any read: a curated tool's from its typed
// arguments, a generic tool's from the operation and steps it names.
func (p *Product) build(ctx context.Context, call growth.Call) (act, error) {
	reason, _ := call.Args["reason"].(string)
	reason = strings.TrimSpace(reason)
	var a act
	var err error
	switch call.Tool {
	case toolHaltRelease:
		a, err = p.haltAct(call)
	case toolRelease:
		a, err = p.releaseAct(call)
	case toolUpdateListing:
		a, err = p.listingAct(call)
	case toolUpdateImages:
		a, err = p.imagesAct(ctx, call)
	default:
		a, err = p.genericAct(ctx, call)
	}
	if err != nil {
		return a, err
	}
	a.reason = reason
	if tool, _ := p.Catalog().Tool(call.Tool); tool.Class.Held() && reason == "" {
		return a, refusal(growth.RefusedInvalid, "`reason` is one line saying why, which the administrator "+
			"reads. No vendor was reached.")
	}
	return a, nil
}

// genericAct is a generic tool's act: a commit of its steps, one staged
// change committed alone, or one plain request.
func (p *Product) genericAct(ctx context.Context, call growth.Call) (act, error) {
	req, err := p.genericRequest(call, call.Args)
	if err != nil {
		return act{}, err
	}
	switch {
	case req.Op.ID == opEditCommit:
		a := act{app: req.Account, edit: true, commit: req.Query}
		raw, _ := call.Args["steps"].([]any)
		if len(raw) == 0 || len(raw) > maxSteps {
			return act{}, refusal(growth.RefusedInvalid, "androidpublisher.edits.commit names 1 to 20 `steps`, "+
				"the changes it stages and commits. No vendor was reached.")
		}
		for _, r := range raw {
			args := objectArg(r)
			s, stepErr := p.stepRequest(args)
			if stepErr != nil {
				return act{}, stepErr
			}
			st := step{req: s}
			if st.media, err = p.mediaOf(ctx, call.Handle, s.Op, args["media"]); err != nil {
				return act{}, err
			}
			a.steps = append(a.steps, st)
		}
		return a, nil
	case staged(req.Op):
		st := step{req: req}
		if st.media, err = p.mediaOf(ctx, call.Handle, req.Op, call.Args["media"]); err != nil {
			return act{}, err
		}
		return act{app: req.Account, edit: true, steps: []step{st}}, nil
	}
	st := step{req: req}
	if st.media, err = p.mediaOf(ctx, call.Handle, req.Op, call.Args["media"]); err != nil {
		return act{}, err
	}
	return act{app: req.AccountOrTarget(), steps: []step{st}}, nil
}

// readBefores reads each step's target as it is now, in edit editID for
// staged changes. A create, and a target with no read method, has none.
func (p *Product) readBefores(ctx context.Context, handle string, a *act, editID string) error {
	for i := range a.steps {
		address, ok := p.beforeAddress(a.steps[i].req)
		if !ok {
			a.steps[i].before = nil
			continue
		}
		if a.edit {
			address = strings.Replace(address, "/edits/"+editSentinel, "/edits/"+editID, 1)
		}
		get := growth.Operation{ID: a.steps[i].req.Op.ID + ".before", Method: "GET", Path: address,
			Class: growth.ClassRead, Permissions: []string{scopePublisher}}
		raw, err := p.client.Send(ctx, handle, googlegrowth.Request{Op: get, Address: address})
		if err != nil {
			return err
		}
		a.steps[i].before = raw
	}
	return nil
}

// beforeAddress is where a step's target is read: its own address without a
// custom verb or the upload prefix, where a read method of the catalog has
// that shape; false where none does, and for a create outside an edit, which
// acts on nothing yet.
func (p *Product) beforeAddress(req googlegrowth.Request) (string, bool) {
	address := strings.Replace(req.Address, publisherAPI+"/upload/", publisherAPI+"/", 1)
	path := strings.Replace(req.Op.Path, publisherAPI+"/upload/", publisherAPI+"/", 1)
	verb := false
	if i := strings.LastIndex(path, ":"); i > strings.LastIndex(path, "}") && i > strings.LastIndex(path, "/") {
		path, address, verb = path[:i], address[:strings.LastIndex(address, ":")], true
	}
	if req.Op.Method == "POST" && !verb && !staged(req.Op) {
		return "", false
	}
	return address, slices.Contains(getTemplates(p.Catalog()), path)
}

// getTemplates are the catalog's read paths, which a target may be read at.
func getTemplates(c growth.Catalog) []string {
	getsOnce.Do(func() {
		for _, op := range c.Operations {
			if op.Method == "GET" && op.Class == growth.ClassRead {
				gets = append(gets, op.Path)
			}
		}
	})
	return gets
}

var (
	getsOnce sync.Once
	gets     []string
)

// binding is what an approval of a binds to: every request as it will be
// sent (the edit's ID left out), every file's digest, and every target's
// state now.
func binding(a act) string {
	h := sha256.New()
	write := func(b []byte) { _, _ = fmt.Fprintf(h, "%d:", len(b)); h.Write(b) }
	write([]byte(a.app))
	commit := ""
	if a.edit {
		commit = commitQuery(a).Encode()
	}
	write([]byte(commit))
	for _, s := range a.steps {
		write([]byte(s.req.Digest(s.before)))
		if s.media != nil {
			write([]byte(s.media.SHA256))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// commitQuery is the query a's commit is sent with: never cancelling a
// submission in review unless the call says so.
func commitQuery(a act) url.Values {
	q := url.Values{}
	for k, v := range a.commit {
		q[k] = v
	}
	if q.Get("changesInReviewBehavior") == "" {
		q.Set("changesInReviewBehavior", errorIfInReview)
	}
	return q
}

// classOfAct is the class a comes to: its steps' highest, and for an edit
// at most operate where its commit is not sent for review.
func (p *Product) classOfAct(a act) growth.Class {
	out := growth.ClassRead
	for i := range a.steps {
		c := classOf(a.steps[i].req, a.steps[i].before)
		a.steps[i].class = c
		if c.Rank() > out.Rank() {
			out = c
		}
	}
	if a.edit {
		q := commitQuery(a)
		switch {
		case q.Get("changesInReviewBehavior") != errorIfInReview:
			// CANCELLING A SUBMISSION IN REVIEW retracts what a person sent.
			if out.Rank() < growth.ClassPublic.Rank() {
				out = growth.ClassPublic
			}
		case q.Get("changesNotSentForReview") == "true" && out == growth.ClassPublic:
			// NOTHING REACHES USERS until a person sends it for review.
			out = growth.ClassOperate
		}
	}
	return out
}

// classOf is the class one request comes to against its target's state
// before: its operation's, lowered by the state rule it meets, raised by a
// field rule it touches.
func classOf(req googlegrowth.Request, before []byte) growth.Class {
	op := req.Op
	var current map[string]any
	_ = json.Unmarshal(before, &current)
	if s, ok := stateHolds(op, req, current); ok {
		op.Class = s
	}
	for _, f := range op.Fields {
		if containsKey(req.Body, f.Field) && f.Class.Rank() > op.Class.Rank() {
			op.Class = f.Class
		}
	}
	return op.Class
}

// stateHolds is the class op's state rule gives req on a target reading
// current, false where no rule holds (the operation's class stands).
func stateHolds(op growth.Operation, req googlegrowth.Request, current map[string]any) (growth.Class, bool) {
	if len(op.States) == 0 {
		return "", false
	}
	rule := op.States[0]
	var holds bool
	switch op.ID {
	case "androidpublisher.edits.tracks.patch", "androidpublisher.edits.tracks.update":
		holds = draftsOrHalts(current, req.Body)
	case "androidpublisher.monetization.subscriptions.patch":
		holds = !anyState(current, "basePlans", "ACTIVE")
	case "androidpublisher.monetization.subscriptions.basePlans.offers.patch":
		state, _ := current["state"].(string)
		holds = state != "ACTIVE"
	case "androidpublisher.monetization.onetimeproducts.patch":
		holds = !anyState(current, "purchaseOptions", "ACTIVE")
	case "androidpublisher.inappproducts.insert", "androidpublisher.inappproducts.patch",
		"androidpublisher.inappproducts.update":
		was, _ := current["status"].(string)
		now, set := req.Body["status"].(string)
		holds = !strings.EqualFold(was, "active") && (!set || !strings.EqualFold(now, "active"))
	case "androidpublisher.orders.reviewrefund":
		pref, _ := req.Body["refundPreference"].(string)
		holds = pref == "APPROVE"
	}
	return rule.Class, holds
}

// anyState reports whether any entry of current's list field is in state.
func anyState(current map[string]any, field, state string) bool {
	list, _ := current[field].([]any)
	return slices.ContainsFunc(list, func(e any) bool {
		m, _ := e.(map[string]any)
		s, _ := m["state"].(string)
		return s == state
	})
}

// containsKey reports whether key is set anywhere in v, at any depth: a tax
// setting inside a batch's requests is still a tax setting.
func containsKey(v any, key string) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, inner := range t {
			if k == key || containsKey(inner, key) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(t, func(e any) bool { return containsKey(e, key) })
	}
	return false
}

// uploads reports whether op sends a file.
func uploads(op growth.Operation) bool { return strings.Contains(op.Path, "/upload/") }
