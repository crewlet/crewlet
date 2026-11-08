package e2e

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
)

// THE GOLDEN COMPANY'S FOUNDER, as a company employs a person.
//
// The company document declares the founder's human seat and a Slack id for
// it, and nobody on it. Inside a turn somebody woke an agent uses only the
// apps that somebody uses (ADR-0109), and a human seat nobody holds in the
// identity directory is somebody the company knows by their contacts alone,
// who uses none — so a case whose subject is what a seat does for the
// founder's word (answers in a room, files a task, searches the pages) employs
// them first, as an administrator does with Settings › People › Add person or
// `crewlet iam create -seat founder -grants …`.

// employFounder creates the person the founder's seat speaks for on on, which
// must hold the identity estate (a data node), active and using every app, and
// waits until on and every member of also that holds the estate reads them
// holding the seat with those apps — by the read a turn composes its requester
// from (iamdomain's Reader.Requester), on the copy of the directory that turn's
// node reads: its own on a data node, a data node's on a node without `data`,
// which then waits on nothing of its own (pass the data node it reads through
// as on or in also).
//
// NOT [engine.Engine.BoundSeat], which answers which seat a TOKEN is bound to
// and reads a machine's row alone: a person is never found there.
func employFounder(t *testing.T, on *node, also ...*node) {
	t.Helper()
	writer := on.engine.IAMWriter().As(iam.Principal{
		Login: iam.TokenLogin(e2eTokenID), Kind: iam.KindMachine,
		Grants: []iam.Grant{iam.GrantAdmin},
	})
	key, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	founder, err := iamdomain.CreatedPersonID(key.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Create(t.Context(), iamdomain.Creation{
		PersonID: founder, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Jane Doe", Email: janeAddress, Login: janeLogin, Seat: janeSeat,
		Grants:        slices.Clone(iam.Apps),
		LinkExpiresAt: time.Now().Add(credential.EnrolmentLinkLifetime),
		OpID:          key.String(), Reason: "the e2e founder, employed so agents act for her word",
	}); err != nil {
		t.Fatalf("employ the founder on %s: %v", on.id, err)
	}
	for _, n := range append([]*node{on}, also...) {
		reader := n.engine.IAM()
		if reader == nil {
			continue
		}
		waitFor(t, n.id+" to read the founder holding her seat with every app", func() bool {
			holder, held, err := reader.Requester(t.Context(), janeSeat)
			if err != nil || !held || holder.Removed || holder.Stage != iam.StageActive {
				return false
			}
			for _, app := range iam.Apps {
				if !slices.Contains(holder.Grants, app) {
					return false
				}
			}
			return true
		})
	}
}

// wakeAsFounder is [wakeWithMessage] from the founder rather than from a
// stranger: a direct message on Slack from the id the company document gives
// her seat (`U0FOUNDER`), which the party registry resolves to it, so the turn
// it wakes is worked for her — with the apps she uses once employed
// ([employFounder]), where a stranger's uses none.
func wakeAsFounder(t *testing.T, n *node, handle string) {
	t.Helper()
	body := "can you send the weekly numbers, plainly, no preamble"
	ev := events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: "Jane Doe", Subject: "weekly numbers",
		Body: body, SalientBody: &body,
		Metadata: map[string]string{
			notify.ActorField:       "U0FOUNDER",
			notify.ChannelKindField: string(types.ChannelDM),
		},
	}, events.TraceContext{})
	if err := n.engine.Backends().Queue.Publish(t.Context(),
		seatInbox(t, n, handle), ev); err != nil {
		t.Fatalf("wake %s as the founder: %v", handle, err)
	}
}
