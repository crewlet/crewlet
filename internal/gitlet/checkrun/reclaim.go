package checkrun

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// attemptRecord is attempt as its box records it ([sandbox.Spec.Check]): the
// key's JSON, which [recordedAttempt] reads back.
func attemptRecord(attempt gitlet.CheckKey) (string, error) {
	raw, err := json.Marshal(attempt)
	return string(raw), err
}

// recordedAttempt is the attempt a box's record names, or why it names none
// this runner reads.
func recordedAttempt(record string) (gitlet.CheckKey, error) {
	var attempt gitlet.CheckKey
	if err := json.Unmarshal([]byte(record), &attempt); err != nil {
		return gitlet.CheckKey{}, err
	}
	return attempt, attempt.Validate()
}

// reclaim kills the boxes on this node an attempt left (package doc, "A box
// its attempt left"): each recording an attempt this runner does not run whose
// row has ended, names another box or is gone, or recording none this runner
// reads. At most [ReclaimPerWalk] are judged, each at most once a poll: a box
// kept, or one that could not be judged or killed, waits a poll for the next.
func (r *Runner) reclaim(ctx context.Context) {
	boxes, ok := r.o.Boxes()
	if !ok || boxes == nil {
		return
	}
	listed, err := boxes.CheckBoxes()
	if err != nil {
		if !r.unlisted {
			r.unlisted = true
			r.log.WarnContext(ctx, "code_check_boxes_unlisted", "error", err.Error(), "detail",
				"this node's check boxes could not be listed, so none an attempt left is reclaimed until they are")
		}
		return
	}
	r.unlisted = false
	on := make(map[string]bool, len(listed))
	judged := 0
	for _, box := range listed {
		on[box.SandboxID] = true
		if judged == ReclaimPerWalk || ctx.Err() != nil {
			continue
		}
		attempt, unread := recordedAttempt(box.Check)
		if unread == nil && r.runs(attempt) {
			continue
		}
		if at, seen := r.judged[box.SandboxID]; seen && time.Since(at) < r.timing.Poll {
			continue
		}
		judged++
		r.judged[box.SandboxID] = time.Now()
		if unread != nil {
			r.kill(ctx, boxes, box, attempt, "its record names no attempt this runner reads: "+unread.Error())
			continue
		}
		why, err := r.left(ctx, box, attempt)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				r.log.WarnContext(ctx, "code_check_box_unjudged", "box", box.SandboxID, "repo_id", attempt.RepoID,
					"check", attempt.Name, "attempt", attempt.Attempt, "error", err.Error(), "detail",
					"its attempt's row could not be read, so the box is judged again a poll later")
			}
		case why != "":
			r.kill(ctx, boxes, box, attempt, why)
		}
	}
	maps.DeleteFunc(r.judged, func(id string, _ time.Time) bool { return !on[id] })
}

// left is why box's attempt has left it, "" where the attempt may still run in
// it: its row running and naming it, or naming no box yet. READ LINEARIZABLE,
// since what it answers kills a box: a copy behind the claim holds no row.
func (r *Runner) left(ctx context.Context, box sandbox.CheckBox, attempt gitlet.CheckKey) (string, error) {
	a, err := r.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadCheckBox, CheckBox: &attempt,
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	switch {
	case err != nil:
		return "", err
	case len(a.Checks) != 1:
		return "its attempt's row is gone", nil
	}
	switch row := a.Checks[0]; {
	case row.State.Ended():
		return fmt.Sprintf("its attempt ended %s", row.State), nil
	case row.Box != "" && row.Box != box.SandboxID:
		return "its attempt runs in box " + row.Box, nil
	}
	return "", nil
}

// kill reclaims box through the backend its record names, UNDER A CONTEXT THE
// RUNNER'S STOP DOES NOT REACH: one cut short would leave a box whose attempt
// is over running until this node's runner starts again. A kill that fails
// keeps the box listed, its record with it ([sandbox.Provider.Kill]), so it is
// never logged reclaimed and is tried again a poll later.
func (r *Runner) kill(ctx context.Context, boxes Boxes, box sandbox.CheckBox, attempt gitlet.CheckKey,
	why string) {
	provider, err := boxes.Provider(box.Placement)
	if err == nil {
		err = provider.Kill(context.WithoutCancel(ctx), box.SandboxID)
	}
	if err != nil {
		r.log.WarnContext(ctx, "code_check_box_reclaim_failed", "box", box.SandboxID,
			"placement", string(box.Placement), "repo_id", attempt.RepoID, "check", attempt.Name,
			"attempt", attempt.Attempt, "error", err.Error(), "detail", why+"; tried again a poll later")
		return
	}
	delete(r.judged, box.SandboxID)
	r.log.InfoContext(ctx, "code_check_box_reclaimed", "box", box.SandboxID, "placement", string(box.Placement),
		"repo_id", attempt.RepoID, "oid", attempt.OID, "check", attempt.Name, "attempt", attempt.Attempt,
		"detail", why)
}
