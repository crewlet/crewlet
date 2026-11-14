package coordtest

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ---- model checks -------------------------------------------------------- //

// modelCheck is a running check of entry on node, started now.
func (h *fleetHarness) modelCheck(entry, node string) coord.ModelCheck {
	return coord.ModelCheck{
		ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("check/"+entry+"/"+node)), Entry: entry, Node: node,
		Smoke: true, StartedAt: h.now(), Deadline: h.now().Add(3 * time.Minute),
		By: "jane-doe", ByKind: iam.ActorHuman, OperatorID: sessionOf("jane-doe"),
	}
}

// answerOf is a JSON answer of exactly n bytes.
func answerOf(n int) json.RawMessage {
	const open, closing = `{"lines":"`, `"}`
	return json.RawMessage(open + strings.Repeat("x", n-len(open)-len(closing)) + closing)
}

var modelCheckCases = []fleetCase{{
	// ONE CHECK OF AN ENTRY AT A TIME across the fleet is the store's own
	// create: a second ask is told one is running, and only an update at the
	// version read can replace it, which is how the node running a check
	// records its end and how an ask over an ended check wins.
	name: "a model check is one per entry, compare and set",
	fn: func(h *fleetHarness) {
		first, created, err := h.f.CreateModelCheck(h.ctx, h.modelCheck("sonnet", "node-1"))
		if err != nil || !created || first.Version == 0 {
			h.t.Fatalf("CreateModelCheck = %+v (%v, %v), want the first check stored", first, created, err)
		}
		if _, again, againErr := h.f.CreateModelCheck(h.ctx, h.modelCheck("sonnet", "node-2")); againErr != nil || again {
			h.t.Fatalf("a second check of the entry = (%v, %v), want it refused", again, againErr)
		}
		ended := first
		ended.EndedAt, ended.Answer = h.now().Add(40*time.Second), json.RawMessage(`{"ok":true,"lines":[]}`)
		moved, ok, err := h.f.UpdateModelCheck(h.ctx, ended)
		if err != nil || !ok || moved.Version == first.Version {
			h.t.Fatalf("UpdateModelCheck at the version read = %+v (%v, %v)", moved, ok, err)
		}
		if _, ok, _ := h.f.UpdateModelCheck(h.ctx, first); ok {
			h.t.Fatal("an update at a superseded version won, taking the check's end back")
		}
		read, found, err := h.f.ModelCheck(h.ctx, "sonnet")
		if err != nil || !found || read.ID != first.ID || read.Node != "node-1" || !read.Smoke || read.Draft ||
			read.Running() || !read.EndedAt.Equal(ended.EndedAt) || !read.Deadline.Equal(first.Deadline) ||
			!bytes.Equal(read.Answer, ended.Answer) || read.ByKind != iam.ActorHuman || read.Version != moved.Version {
			h.t.Fatalf("ModelCheck = %+v (found %v, %v), want node-1's ended check", read, found, err)
		}
		if gone, _ := h.f.DeleteModelCheck(h.ctx, "sonnet", first.Version); gone {
			h.t.Fatal("a removal at a superseded version won")
		}
		if gone, err := h.f.DeleteModelCheck(h.ctx, "sonnet", moved.Version); err != nil || !gone {
			h.t.Fatalf("DeleteModelCheck at the current version = (%v, %v)", gone, err)
		}
		if _, found, err := h.f.ModelCheck(h.ctx, "sonnet"); err != nil || found {
			h.t.Fatalf("a removed check still reads (found %v, %v)", found, err)
		}
		if _, created, _ := h.f.CreateModelCheck(h.ctx, h.modelCheck("sonnet", "node-2")); !created {
			h.t.Fatal("an entry whose check was removed could not be checked again")
		}
	},
}, {
	name: "a model check names its id, entry, node, lifetime and author, and ends with an answer or a failure",
	fn: func(h *fleetHarness) {
		good := h.modelCheck("sonnet", "node-1")
		ended := func(c *coord.ModelCheck) { c.EndedAt = c.StartedAt.Add(time.Second) }
		for name, mutate := range map[string]func(*coord.ModelCheck){
			"no id":               func(c *coord.ModelCheck) { c.ID = uuid.Nil },
			"no entry":            func(c *coord.ModelCheck) { c.Entry = "" },
			"no node":             func(c *coord.ModelCheck) { c.Node = "" },
			"no start":            func(c *coord.ModelCheck) { c.StartedAt = time.Time{} },
			"a deadline at start": func(c *coord.ModelCheck) { c.Deadline = c.StartedAt },
			"no author":           func(c *coord.ModelCheck) { c.By = "" },
			"no kind":             func(c *coord.ModelCheck) { c.ByKind = "" },
			"running with an answer": func(c *coord.ModelCheck) {
				c.Answer = json.RawMessage(`{}`)
			},
			"running with a failure": func(c *coord.ModelCheck) { c.Failure = "the node stopped" },
			"ended with neither":     ended,
			"ended with both": func(c *coord.ModelCheck) {
				ended(c)
				c.Answer, c.Failure = json.RawMessage(`{}`), "the node stopped"
			},
			"ended before it started": func(c *coord.ModelCheck) {
				c.EndedAt, c.Failure = c.StartedAt.Add(-time.Second), "the node stopped"
			},
			"an answer past its bound": func(c *coord.ModelCheck) {
				ended(c)
				c.Answer = answerOf(coord.MaxModelCheckAnswerBytes + 1)
			},
			"an answer that is not JSON": func(c *coord.ModelCheck) {
				ended(c)
				c.Answer = json.RawMessage(`{"lines":`)
			},
			"a failure past its bound": func(c *coord.ModelCheck) {
				ended(c)
				c.Failure = strings.Repeat("x", coord.MaxModelCheckFailureBytes+1)
			},
		} {
			bad := good
			mutate(&bad)
			if _, created, err := h.f.CreateModelCheck(h.ctx, bad); err == nil || created {
				h.t.Errorf("%s: CreateModelCheck = (%v, %v), want a refusal", name, created, err)
			}
		}
		if all, _ := h.f.ListModelChecks(h.ctx); len(all) != 0 {
			h.t.Errorf("a refused check left %+v behind", all)
		}
	},
}, {
	// THE BOUND IS THE STORE'S TO CARRY: an answer the writer cut to the
	// bound is read back byte for byte, so a page shows what the check found
	// and never a store's truncation of it.
	name: "a model check's answer at its bound reads back whole",
	fn: func(h *fleetHarness) {
		c := h.modelCheck("sonnet", "node-1")
		c.EndedAt, c.Answer = c.StartedAt.Add(time.Minute), answerOf(coord.MaxModelCheckAnswerBytes)
		if _, created, err := h.f.CreateModelCheck(h.ctx, c); err != nil || !created {
			h.t.Fatalf("CreateModelCheck at the answer's bound = (%v, %v)", created, err)
		}
		read, found, err := h.f.ModelCheck(h.ctx, "sonnet")
		if err != nil || !found || !bytes.Equal(read.Answer, c.Answer) {
			h.t.Fatalf("ModelCheck = %d bytes of answer (found %v, %v), want the %d stored", len(read.Answer),
				found, err, len(c.Answer))
		}
	},
}, {
	name: "model checks list by entry",
	fn: func(h *fleetHarness) {
		for _, entry := range []string{"sonnet", "codex-review", "opus"} {
			if _, created, err := h.f.CreateModelCheck(h.ctx, h.modelCheck(entry, "node-1")); err != nil || !created {
				h.t.Fatalf("CreateModelCheck(%s) = (%v, %v)", entry, created, err)
			}
		}
		all, err := h.f.ListModelChecks(h.ctx)
		if err != nil {
			h.t.Fatalf("ListModelChecks: %v", err)
		}
		var entries []string
		for _, c := range all {
			if c.Version == 0 {
				h.t.Errorf("listed %s with no version", c.Entry)
			}
			entries = append(entries, c.Entry)
		}
		if strings.Join(entries, ",") != "codex-review,opus,sonnet" {
			h.t.Fatalf("ListModelChecks = %v, want every check by entry", entries)
		}
	},
}}
