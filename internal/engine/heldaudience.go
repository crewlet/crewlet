package engine

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run that holds a call for approval is put to the people its tool's
// approver policy admits, never to the seat's manager chain as such and never
// to an agent: no agent may approve a call, and a question an agent receives
// is one nobody who may answer it sees. An SSH operate command's approvers are
// the administrators and the people integrations.ssh.approvers names
// (ssh.operate.approve); any other held call's are the people among the
// seat's leads and the administrators (tools.approve).

// heldAudienceRead bounds the directory read that names the administrators.
const heldAudienceRead = 10 * time.Second

// heldAudience is who run's held call is put to: the people its approver
// policy names, then every administrator holding a seat, each once, people
// alone, in that order.
func heldAudience(company *Company, run sandbox.PendingRun, admins []string) sandbox.Audience {
	o := company.Org
	var out []string
	add := func(handle string) {
		r := o.Role(handle)
		if r == nil || !r.IsHuman() || slices.Contains(out, r.Handle()) {
			return
		}
		out = append(out, r.Handle())
	}
	if run.HeldCall.Approvers == sandbox.HeldApproversSSH {
		if company.Config != nil && company.Config.Integrations.SSH != nil {
			for _, handle := range company.Config.Integrations.SSH.Approvers {
				add(handle)
			}
		}
	} else {
		for _, handle := range leadChain(o, o.SeatByHandle(run.AgentHandle)) {
			add(handle)
		}
	}
	for _, handle := range admins {
		add(handle)
	}
	return sandbox.Audience{Handles: out}
}

// adminSeats is the seat of every administrator who may act, read off this
// node's identity directory; what was read before a failed page where it
// cannot read the rest, which the log says.
func (e *Engine) adminSeats() []string {
	reader := e.IAM()
	if reader == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), heldAudienceRead)
	defer cancel()
	var out []string
	for after := ""; ; {
		page, err := reader.People(ctx, iamdomain.PeopleQuery{After: after, Limit: iamdomain.MaxPageSize,
			Stage: iam.StageActive})
		if err != nil {
			log.Warn("held_call_audience_admins_unread", "error", err.Error(),
				"detail", "a held call is put to the administrators read so far and its approvers")
			return out
		}
		for _, p := range page.People {
			if p.Kind == iam.KindPerson && p.Seat != "" && slices.Contains(p.Grants, iam.GrantAdmin) {
				out = append(out, p.Seat)
			}
		}
		if page.Next == "" {
			return out
		}
		after = page.Next
	}
}
