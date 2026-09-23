package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SATELLITE TAKES THE MOST-CAUGHT-UP ANSWER.
//
// Two nodes run the identity domain and answer from their own rows, one of them
// behind: it has not applied the founder's suspension yet. The satellite's
// reading must be the one read at the higher position — the one closer to the
// log — so the suspension reaches the satellite as soon as ANY node holding
// the directory has applied it, rather than when whichever replied first had.
func TestASatelliteTakesTheMostCaughtUpAnswer(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	servingNode(t, broker, "node-a", 5, iam.StageActive)
	servingNode(t, broker, "node-b", 9, iam.StageSuspended)

	sat := satellite(t, broker, 2, false)
	standing, err := notify.ReadStanding(t.Context(), sat)
	if err != nil {
		t.Fatalf("read the fleet's directory: %v", err)
	}
	o := directoryCompany(t, &Engine{}).Org
	if why, ok := standing.Seats(o)[founderSeat]; !ok || why != notify.WithheldSuspended {
		t.Errorf("the satellite reads the founder as %q/%v, want the suspension "+
			"the caught-up node has applied", why, ok)
	}
}

// NOBODY TO ASK AND A LOG WITH NOTHING IN IT IS AN EMPTY DIRECTORY — a fact.
//
// That is an all-seats fleet: nobody runs the domain, nothing was ever
// published to its log, so nobody is bound to anything anywhere. Reading it as
// unknown would withhold every human seat in a deployment that has no sign-in
// surface and so nobody to suspend.
func TestNobodyToAskOverAQuietLogIsAnEmptyDirectory(t *testing.T) {
	t.Parallel()
	sat := satellite(t, memory.NewBroker(), 0, true)
	standing, err := notify.ReadStanding(t.Context(), sat)
	if err != nil {
		t.Fatalf("an all-seats fleet's directory read as %v", err)
	}
	if !standing.Consulted() || len(standing.Withheld()) != 0 {
		t.Errorf("read %+v, want a consulted reading withholding nothing", standing)
	}
}

// NOBODY ANSWERING OVER A LOG WITH RECORDS IS UNKNOWN — never an empty reading.
//
// Some node ran the domain, so somebody may be suspended, and the one answer
// the satellite must not give is "nobody is". The same holds when the
// answerers are counted as live but none replies in the budget, and when the
// presence listing itself cannot be read.
func TestASatelliteThatHearsNobodyOverARecordedLogDoesNotGuess(t *testing.T) {
	t.Parallel()
	for name, sat := range map[string]*fleetDirectory{
		"nobody live":          satellite(t, memory.NewBroker(), 0, false),
		"live but silent":      satellite(t, memory.NewBroker(), 1, false),
		"presence unreadable":  {answerers: func(context.Context) (int, error) { return 0, errBoom }},
		"log state unreadable": {answerers: func(context.Context) (int, error) { return 0, nil }, quiet: func(context.Context) (bool, error) { return false, errBoom }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := notify.ReadStanding(t.Context(), sat); err == nil {
				t.Error("a satellite that could not learn the fleet's directory " +
					"answered with a reading")
			}
		})
	}
}

// THE ANSWER NAMES SEATS AND STAGES AND NEVER A PERSON.
//
// It crosses the broker to a node that deliberately holds no directory, so it
// carries exactly the fact the satellite's registry needs — which handle a
// binding names, and at what stage — and not who holds it.
func TestTheDirectoryAnswerCarriesNoPerson(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	src := holdersSourceFunc{
		at: statelog.Position{Seq: 3},
		holders: []iamdomain.SeatHolder{{
			Seat: founderSeat, Person: "018f3a9c-0000-7000-8000-00000000beef",
			Stage: iam.StageActive,
		}},
	}
	serve(t, broker, "node-a", src)
	asker := started(t, broker)
	request, err := json.Marshal(holdersRequest{Version: holdersProtocol})
	if err != nil {
		t.Fatal(err)
	}
	replies, err := asker.Ask(t.Context(), topics.IamHolders, request, 1)
	if err != nil || len(replies) != 1 {
		t.Fatalf("ask: %d replies, %v", len(replies), err)
	}
	if strings.Contains(string(replies[0]), "beef") {
		t.Errorf("the answer carries the person bound to the seat: %s", replies[0])
	}
	if !strings.Contains(string(replies[0]), founderSeat) {
		t.Errorf("the answer does not name the seat: %s", replies[0])
	}
}

var errBoom = errors.New("coordination is unreachable")

// servingNode is one node running the identity domain at a position, holding
// the founder at a stage.
func servingNode(t *testing.T, broker *memory.Broker, node string, seq uint64,
	stage iam.Stage) {
	t.Helper()
	serve(t, broker, node, holdersSourceFunc{
		at: statelog.Position{Stream: topics.IamLogStream, Seq: seq},
		holders: []iamdomain.SeatHolder{
			{Seat: founderSeat, Stage: stage},
			{Seat: colleagueSeat, Stage: iam.StageActive},
		},
	})
}

func serve(t *testing.T, broker *memory.Broker, node string, src holdersSource) *memory.Queue {
	t.Helper()
	q := started(t, broker)
	stop, err := serveHolders(t.Context(), q, node, src)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return q
}

// satellite is a node running no identity domain on this broker, counting
// want live answerers over a log that is quiet or not.
func satellite(t *testing.T, broker *memory.Broker, want int, quiet bool) *fleetDirectory {
	t.Helper()
	return &fleetDirectory{
		ask:       started(t, broker),
		answerers: func(context.Context) (int, error) { return want, nil },
		quiet:     func(context.Context) (bool, error) { return quiet, nil },
	}
}

func started(t *testing.T, broker *memory.Broker) *memory.Queue {
	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	return q
}

// holdersSourceFunc is a directory at a fixed position.
type holdersSourceFunc struct {
	at      statelog.Position
	holders []iamdomain.SeatHolder
}

func (s holdersSourceFunc) SeatHolders(context.Context) ([]iamdomain.SeatHolder, error) {
	return s.holders, nil
}

func (s holdersSourceFunc) At() statelog.Position { return s.at }
