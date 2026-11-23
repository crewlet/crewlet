package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// unlisted is a fleet whose seat access ledgers cannot be listed: a reader of
// one seat's record through a listing fails on it.
// (Embedded under an alias: coord.Fleet declares a method named Fleet.)
type unlisted struct{ wholeFleet }

type wholeFleet = coord.Fleet

func (unlisted) SeatAccessRecords(context.Context, string) (map[string][]byte, error) {
	return nil, errors.New("a reader of one seat's record listed its integration's ledger")
}

// ONE SEAT'S ACCESS RECORD IS READ BY ITS KEY wherever one seat's is asked for:
// a Google Cloud token, issued on every launch and every 15 s poll of a
// running box, and a Slack install link. A listing of the integration's
// ledger would carry every other seat's record, each up to
// coord.MaxSeatAccessBytes, across a node's link, and fail on any other
// seat's unreadable one.
//
// Mutation: read either through ledger.Records, and it fails on the listing.
func TestOneSeatsAccessRecordIsReadByItsKey(t *testing.T) {
	t.Parallel()
	g := newSeatGoogle(t)
	c := googleCloudCompany(t)
	e := googleCloudEngine(t, c, g, time.Now().Add(-time.Hour))
	e.backends.Fleet = unlisted{wholeFleet: e.backends.Fleet}
	issue := e.seatIssuers()[0].issuing(c, c.Org.AgentSeatByHandle("builder"), e.resolver())
	if issue == nil {
		t.Fatal("premise: builder is on and issued nothing")
	}
	if _, err := issue(t.Context(), toolTokenLife); err != nil {
		t.Errorf("the Google Cloud token: %v", err)
	}

	fleet := coordmemory.NewFleet()
	slack := &Engine{backends: &Backends{Fleet: unlisted{wholeFleet: fleet}}}
	ledger, err := slack.SeatAccessLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Put(t.Context(), seataccess.Record{Kind: integration.KindSlack, Handle: "builder",
		Account: &seataccess.Account{ID: "A0APP", CreatedAt: time.Now(), CreatedByPass: "test",
			State: seataccess.AccountActive}}); err != nil {
		t.Fatal(err)
	}
	api := &SeatAccessAPI{engine: slack,
		installLink: func(_ context.Context, handle, app string) (string, error) {
			return "https://slack.com/oauth/v2/authorize?app=" + app + "&seat=" + handle, nil
		},
		awaitsInstall: func(seataccess.Record) bool { return true }}
	link, err := api.InstallLink(t.Context(), setupapi.AccessQuery{Kind: integration.KindSlack}, "builder")
	if err != nil || link == "" {
		t.Errorf("the Slack install link = %q, %v", link, err)
	}
}
