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
	"github.com/crewlet/crewlet/internal/queue"
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
		"log state unreadable": {answerers: func(context.Context) (int, error) { return 0, nil }, head: func(context.Context) (uint64, error) { return 0, errBoom }},
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
	request, err := json.Marshal(holdersRequest{Version: holdersProtocol, Nonce: "n"})
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

// A SATELLITE'S READING NEVER GOES BACKWARD.
//
// The node that had applied the founder's suspension answers first; then it is
// gone, and the only node answering is one still behind it. Taken as the best
// of that scatter, the behind answer would route the suspended founder again
// until the first node came back — so it is the unknown arm instead, and the
// registry keeps the reading it has. The control: an answer at a HIGHER
// position is taken, which is what lets a reinstatement reach the satellite.
func TestASatellitesReadingNeverGoesBackward(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	stopAhead := servingNode(t, broker, "node-a", 9, iam.StageSuspended)
	sat := satellite(t, broker, 1, false)
	o := directoryCompany(t, &Engine{}).Org

	standing, err := notify.ReadStanding(t.Context(), sat)
	if err != nil {
		t.Fatalf("read the fleet's directory: %v", err)
	}
	if why := standing.Seats(o)[founderSeat]; why != notify.WithheldSuspended {
		t.Fatalf("the founder reads %q, want suspended", why)
	}

	if err := stopAhead(context.Background()); err != nil {
		t.Fatalf("stop the caught-up node: %v", err)
	}
	stopBehind := servingNode(t, broker, "node-b", 5, iam.StageActive)
	if _, err := sat.SeatHolders(t.Context()); !errors.Is(err, errDirectoryBehind) {
		t.Errorf("an answer behind this node's reading answered %v, want %v — "+
			"taken, it routes a suspended person until the node that applied "+
			"the suspension comes back", err, errDirectoryBehind)
	}

	// THE SAME NODE, CAUGHT UP past the reading, with the founder reinstated.
	if err := stopBehind(context.Background()); err != nil {
		t.Fatalf("stop the behind node: %v", err)
	}
	servingNode(t, broker, "node-b", 12, iam.StageActive)
	standing, err = notify.ReadStanding(t.Context(), sat)
	if err != nil {
		t.Fatalf("an answer ahead of the reading was refused: %v", err)
	}
	if why, ok := standing.Seats(o)[founderSeat]; ok {
		t.Errorf("the founder still reads %q after a reinstatement at a later "+
			"position", why)
	}
}

// AN EMPTY LOG UNDER A READING ALREADY TAKEN IS NOT AN EMPTY DIRECTORY.
//
// "Nobody answers over a log that never carried a record" is a fact only while
// this node has never read the directory off that log. Once it has, an empty
// log is one that was recreated or purged underneath it — and reading that as
// "nobody holds any seat" would route every suspended person's accounts.
func TestAnEmptyLogUnderAReadingAlreadyTakenIsUnknown(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	stop := servingNode(t, broker, "node-a", 9, iam.StageSuspended)
	sat := satellite(t, broker, 1, false)
	if _, err := sat.SeatHolders(t.Context()); err != nil {
		t.Fatalf("read the fleet's directory: %v", err)
	}
	if err := stop(context.Background()); err != nil {
		t.Fatalf("stop the node: %v", err)
	}
	sat.head = func(context.Context) (uint64, error) { return 0, nil }
	if holders, err := sat.SeatHolders(t.Context()); !errors.Is(err, errNoDirectoryAnswer) {
		t.Errorf("an emptied log under a reading answered %v, %v; want %v",
			holders, err, errNoDirectoryAnswer)
	}
}

// AN ANSWER THE FLEET DID NOT SIGN IS NOT READ.
//
// The broker authenticates nothing, so anything that can reach it can serve the
// directory's subject. A forger answering one position ahead of the honest node
// with the founder active would be the most caught-up reply, and every
// satellite would route the suspended founder's accounts back to their seat —
// signed under a key the fleet does not hold, or not signed at all, it is a
// reply that never arrived.
func TestAnAnswerTheFleetDidNotSignIsNotRead(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	servingNode(t, broker, "node-a", 9, iam.StageSuspended)

	stranger, err := statelog.NewSigner(holdersSignatureLabel,
		statelog.OneKey("k1", "material-the-fleet-does-not-hold"))
	if err != nil {
		t.Fatal(err)
	}
	serveWith(t, broker, "forger-keyed", founderAt(10, iam.StageActive), stranger)
	serveRaw(t, broker, func(req holdersRequest) []byte {
		return mustJSON(t, holdersReply{Version: holdersProtocol, Node: "forger-plain",
			At: 11, Nonce: req.Nonce, Holders: []holderWire{{Seat: founderSeat,
				Stage: string(iam.StageActive)}}})
	})

	assertFounderSuspended(t, satellite(t, broker, 3, false))
}

// A SIGNED ANSWER FROM ANOTHER SCATTER, OR PAST THE LOG'S HEAD, IS NOT READ.
//
// A signature says the fleet wrote the bytes, not that they answer THIS
// question: an answer captured from an earlier scatter echoes an earlier
// nonce, and one claiming records the identity log does not hold is not a
// reading any node applied — a buggy node, or a key that leaked. Either,
// chosen as the most caught up, would override every honest node.
func TestASignedAnswerThatDoesNotAnswerThisScatterIsNotRead(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	servingNode(t, broker, "node-a", 9, iam.StageSuspended)
	signer := testHoldersSigner(t)
	serveRaw(t, broker, func(holdersRequest) []byte {
		return signer.Seal(mustJSON(t, holdersReply{Version: holdersProtocol,
			Node: "replay", At: 10, Nonce: "a-nonce-from-an-earlier-scatter",
			Holders: []holderWire{{Seat: founderSeat, Stage: string(iam.StageActive)}}}))
	})
	serveWith(t, broker, "beyond-the-head",
		founderAt(testLogHead+1, iam.StageActive), signer)

	assertFounderSuspended(t, satellite(t, broker, 3, false))
}

// testLogHead is the identity log's last sequence as a test satellite reads it.
const testLogHead = 1000

// assertFounderSuspended reads the fleet's directory and requires the honest
// node's suspension.
func assertFounderSuspended(t *testing.T, sat *fleetDirectory) {
	t.Helper()
	standing, err := notify.ReadStanding(t.Context(), sat)
	if err != nil {
		t.Fatalf("read the fleet's directory: %v", err)
	}
	o := directoryCompany(t, &Engine{}).Org
	if why := standing.Seats(o)[founderSeat]; why != notify.WithheldSuspended {
		t.Errorf("the founder reads %q, want the honest node's suspension", why)
	}
}

// founderAt is a directory at seq holding the founder at a stage.
func founderAt(seq uint64, stage iam.Stage) holdersSourceFunc {
	return holdersSourceFunc{
		at: statelog.Position{Stream: topics.IamLogStream, Seq: seq},
		holders: []iamdomain.SeatHolder{
			{Seat: founderSeat, Stage: stage},
			{Seat: colleagueSeat, Stage: iam.StageActive},
		},
	}
}

// serveRaw answers the directory's subject with whatever answer makes, as
// anything that can reach the broker can.
func serveRaw(t *testing.T, broker *memory.Broker, answer func(holdersRequest) []byte) {
	t.Helper()
	q := started(t, broker)
	stop, err := q.Serve(t.Context(), topics.IamHolders,
		func(_ context.Context, raw []byte) ([]byte, error) {
			var req holdersRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			return answer(req), nil
		})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// testHoldersRing is the fleet keyring every test node and satellite shares.
var testHoldersRing = statelog.OneKey("k1", "material-one")

func testHoldersSigner(t *testing.T) *statelog.Signer {
	t.Helper()
	signer, err := statelog.NewSigner(holdersSignatureLabel, testHoldersRing)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

var errBoom = errors.New("coordination is unreachable")

// servingNode is one node running the identity domain at a position, holding
// the founder at a stage, and the way to stop it answering.
func servingNode(t *testing.T, broker *memory.Broker, node string, seq uint64,
	stage iam.Stage) queue.Unsubscribe {
	t.Helper()
	return serve(t, broker, node, founderAt(seq, stage))
}

func serve(t *testing.T, broker *memory.Broker, node string, src holdersSource) queue.Unsubscribe {
	t.Helper()
	return serveWith(t, broker, node, src, testHoldersSigner(t))
}

// serveWith is [serve] signing under a keyring of the caller's choosing.
func serveWith(t *testing.T, broker *memory.Broker, node string, src holdersSource,
	signer *statelog.Signer) queue.Unsubscribe {
	t.Helper()
	q := started(t, broker)
	stop, err := serveHolders(t.Context(), q, node, src, signer)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return stop
}

// satellite is a node running no identity domain on this broker, counting
// want live answerers over a log that is quiet — never written — or holds
// [testLogHead] records.
func satellite(t *testing.T, broker *memory.Broker, want int, quiet bool) *fleetDirectory {
	t.Helper()
	verifier, err := statelog.NewVerifier(holdersSignatureLabel, testHoldersRing)
	if err != nil {
		t.Fatal(err)
	}
	head := uint64(testLogHead)
	if quiet {
		head = 0
	}
	return &fleetDirectory{
		ask:       started(t, broker),
		answerers: func(context.Context) (int, error) { return want, nil },
		head:      func(context.Context) (uint64, error) { return head, nil },
		verifier:  verifier,
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
