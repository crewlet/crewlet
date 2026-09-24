package tracker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A CREATE'S KEY COMES FROM THE COUNTER, AND ITS RANK COMES FROM THE KEY.
//
// # What this pins that no unit test can
//
// A create is TWO appends on two subjects: the project's counter, then the
// task. The number the counter's own arbitration hands out is what makes two
// nodes minting at once produce two keys rather than one — and the rank is
// derived from that same number, which is what makes a create's key a pure
// integer at or above the origin and therefore provably outside every key a
// drag can mint.
//
// Both halves are only observable end to end: the counter's number is formed
// inside a decision the framework may run several times, and the rank is a
// column the applier writes.
func TestACreateTakesItsKeyAndRankFromTheCounter(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for i, id := range []string{"t-1", "t-2", "t-3"} {
		task := newTask(id)
		task.Key = ""
		result, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil)
		if err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		want := []string{"ENG-1", "ENG-2", "ENG-3"}[i]
		if result.Key != want {
			t.Fatalf("the %s create minted key %q, want %q — the number comes "+
				"from the counter's own arbitration, so consecutive creates "+
				"take consecutive numbers", id, result.Key, want)
		}
		if !result.Rank.FromCreate() {
			t.Errorf("%s's rank %q is not a key only a create can have minted "+
				"— the whole disjointness argument is about the SHAPE of the "+
				"value, so a create that mints anything else can collide with "+
				"a drag", id, result.Rank)
		}
		r.drain()
	}

	// AND NEW TASKS LAND AT THE TAIL IN CREATION ORDER, which is the
	// property the counter's monotonicity buys and a fractional mint
	// would not.
	answer := r.ask(map[string]any{"container": "project:ENG", "sort": "rank"})
	var keys []string
	for _, row := range answer.Rows {
		keys = append(keys, row.Key)
	}
	if strings.Join(keys, ",") != "ENG-1,ENG-2,ENG-3" {
		t.Fatalf("the board reads %v in rank order, and creates must land at "+
			"the tail in the order they were made", keys)
	}
}

// A CREATE INTO AN ARCHIVED PROJECT IS REFUSED BEFORE THE COUNTER MOVES.
//
// The refusal has to happen in the counter mint's own decision, not after it:
// a create refused between the two appends has already advanced the project's
// numbering for a task that will never exist.
func TestACreateIntoAnArchivedProjectMovesNoCounter(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	archiveENG(t, r)

	before := counterOf(t, r, "ENG")
	_, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil)
	if err == nil {
		t.Fatal("a create into an archived project was accepted")
	}
	if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("the refusal is %v and does not say the project is archived", err)
	}
	r.drain()
	if after := counterOf(t, r, "ENG"); after != before {
		t.Fatalf("the counter moved from %d to %d for a create that was "+
			"refused — a refusal after the mint burns a key number for a task "+
			"nobody will ever see", before, after)
	}
}

// A REQUIRED FIELD IS REQUIRED ON A TASK AND NOT ON A SUBTASK.
//
// ClickUp ships two toggles and so does this, and the default that matters is
// the second: one required field on a project would otherwise block every
// checklist item anybody promotes into a subtask.
func TestARequiredFieldDoesNotBlockASubtask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	requireAField(t, r)

	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err == nil {
		t.Fatal("a task missing a required field was created")
	} else if !strings.Contains(err.Error(), "requires") {
		t.Fatalf("the refusal is %v and does not name the field", err)
	}
	r.drain()

	parent := "t-parent"
	subtask := newTask("t-2")
	subtask.Parent = &parent
	if _, err := r.writer.CreateTask(t.Context(), "op-2", subtask, nil); err != nil {
		t.Fatalf("a SUBTASK missing the same field was refused: %v — the "+
			"subtask toggle defaults off precisely so a promotion is not "+
			"blocked by its project's own policy", err)
	}
}

// A PROMOTED ITEM IS A SUBTASK WITH A KEY, AND ITS PARENT IS MARKED LAST.
//
// The order is the point: marking the parent first leaves a struck-through
// line pointing at a subtask that does not exist, which no reader can tell
// from one somebody purged.
func TestAPromotedItemIsASubtaskAndItsParentPointsAtIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	parent := newTask("t-1")
	parent.Checklists = []tracker.Checklist{{
		ID: "l-1", Name: "steps",
		Items: []tracker.ChecklistItem{{ID: "i-1", Name: "wire it"}},
	}}
	if _, err := r.writer.CreateTask(t.Context(), "op-1", parent, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	subtask := newTask("t-2")
	subtask.Title = "wire it"
	result, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1", subtask, nil)
	if err != nil {
		t.Fatalf("PromoteItem: %v", err)
	}
	if result.Key != "ENG-2" {
		t.Fatalf("the promotion minted key %q, and a promoted item is a "+
			"subtask with a key exactly as a create is", result.Key)
	}
	r.drain()

	stored := taskOf(t, r, "t-1")
	if len(stored.Checklists) != 1 || len(stored.Checklists[0].Items) != 1 {
		t.Fatalf("the parent's checklist is %+v", stored.Checklists)
	}
	item := stored.Checklists[0].Items[0]
	if item.PromotedTo == nil || *item.PromotedTo != "t-2" {
		t.Fatalf("the item points at %v, not at the subtask it became — the "+
			"line is kept and struck through rather than deleted, which is "+
			"what records that this line became that task", item.PromotedTo)
	}
	child := taskOf(t, r, "t-2")
	if child.Parent == nil || *child.Parent != "t-1" {
		t.Fatalf("the subtask's parent is %v, want t-1", child.Parent)
	}
}

// A RE-RUN OF A PROMOTION COMPLETES IT RATHER THAN DUPLICATING IT.
//
// The subtask's id is derived from the item, so the create is refused on its
// own guarding row and the retry proceeds to the parent commit the first
// attempt did not reach. Without that, the interrupted promotion's repair
// would be a second subtask.
func TestARerunPromotionMarksTheParentItDidNotReach(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	parent := newTask("t-1")
	parent.Checklists = []tracker.Checklist{{
		ID: "l-1", Name: "steps",
		Items: []tracker.ChecklistItem{{ID: "i-1", Name: "wire it"}},
	}}
	if _, err := r.writer.CreateTask(t.Context(), "op-1", parent, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	subtask := newTask("t-2")
	if _, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1", subtask, nil); err != nil {
		t.Fatalf("PromoteItem: %v", err)
	}
	r.drain()
	// THE SAME PROMOTION AGAIN, which is what a re-run after a crash
	// between the subtask and the parent commit does.
	if _, err := r.writer.PromoteItem(t.Context(), "op-3", "t-1", "i-1", subtask, nil); err != nil {
		t.Fatalf("the re-run: %v", err)
	}
	r.drain()

	answer := r.ask(map[string]any{"container": "project:ENG"})
	if len(answer.Rows) != 2 {
		t.Fatalf("the project holds %d tasks after one promotion run twice — "+
			"the subtask's id is derived from the item precisely so the "+
			"second run finds its own subtask", len(answer.Rows))
	}
}

// A BULK EDIT IS NOT ATOMIC AND SAYS SO PER TASK.
//
// It never was atomic: the caller re-runs the failures, and a result that
// reported one outcome for the batch would make a caller re-run tasks that
// changed.
func TestABulkEditReportsEveryTaskSeparately(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}

	done := tracker.StatusDone
	result, err := r.writer.UpdateTasks(t.Context(), "op-bulk",
		[]string{"t-1", "t-2", "t-missing"}, "ENG",
		tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
	if err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if len(result.Applied) != 2 {
		t.Fatalf("the batch applied %v, want the two tasks that exist", result.Applied)
	}
	if _, failed := result.Failed["t-missing"]; !failed {
		t.Fatalf("the batch reported no failure for a task that is not there: "+
			"%+v — a caller that cannot tell which half landed re-runs the "+
			"half that did", result.Failed)
	}
	r.drain()
	answer := r.ask(map[string]any{"container": "project:ENG", "show_closed": "true"})
	closed := 0
	for _, row := range answer.Rows {
		if row.Status == tracker.StatusDone {
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("%d of %d tasks are done after a bulk close of two: %+v",
			closed, len(answer.Rows), answer.Rows)
	}
}

// A BULK EDIT OVER ITS OWN CEILING IS REFUSED BEFORE THE FIRST APPEND.
//
// A bulk refused halfway is a caller told "failed" about tasks that changed,
// which is the one refusal that has to come before any of it lands.
func TestABulkEditIsRefusedBeforeAnythingLands(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	ids := make([]string, tracker.MaxBulkTasks+1)
	for i := range ids {
		ids[i] = "t-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, err := r.writer.UpdateTasks(t.Context(), "op-bulk", ids, "ENG",
		tracker.TaskPatch{Title: ptr("x")}, tracker.ChangeFields, nil); err == nil {
		t.Fatal("a batch over the task ceiling was accepted")
	}

	// A MAXIMAL PATCH over a full batch: the ceiling is on what the batch
	// APPLIES, so the same task count with a larger patch is refused where
	// a small one is not.
	fields := map[string]json.RawMessage{}
	for i := range tracker.MaxFieldValues {
		fields[fmt.Sprintf("f%03d", i)] = json.RawMessage(
			`"` + strings.Repeat("x", tracker.MaxFieldValueBytes) + `"`)
	}
	if _, err := r.writer.UpdateTasks(t.Context(), "op-bytes",
		ids[:tracker.MaxBulkTasks], "ENG",
		tracker.TaskPatch{Fields: &fields}, tracker.ChangeFields, nil); err == nil {
		t.Fatal("a batch over the byte ceiling was accepted")
	} else if !strings.Contains(err.Error(), "bytes of commits") {
		t.Fatalf("the refusal is %v and does not say what the ceiling is "+
			"about — the caller's remedy is fewer tasks per call, which it "+
			"cannot work out from a bare refusal", err)
	}
	r.drain()
	if answer := r.ask(map[string]any{"container": "project:ENG"}); answer.Rows[0].Title != "a task" {
		t.Fatalf("a refused batch changed a row: %q — the pre-flight runs "+
			"before the first append precisely so it cannot", answer.Rows[0].Title)
	}
}

// A BODY NAMING MORE KEYS THAN THE APPLIER LINKS IS WARNED ABOUT, NOT REFUSED.
//
// THE APPLY MUST NOT DEPEND ON THE WARNING. Every node resolves the same first
// sixty-four keys in document order, so the cap is deterministic without it —
// the warning exists so the person who wrote the body learns that the rest are
// plain text, rather than discovering it on a page.
func TestAnOverReferencedBodyWarnsAndStillWrites(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	var body strings.Builder
	for i := range tracker.MaxReferencesPerBody + 10 {
		body.WriteString("see ENG-")
		body.WriteString(string(rune('0' + i%10)))
		body.WriteString(string(rune('0' + i/10)))
		body.WriteString(" ")
	}
	task := newTask("t-1")
	task.Body = body.String()
	result, err := r.writer.CreateTask(t.Context(), "op-1", task, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("a body naming more keys than the applier links produced no " +
			"warning, so the author learns nothing about the ones that are " +
			"plain text")
	}
	r.drain()
	if answer := r.ask(map[string]any{"container": "project:ENG"}); len(answer.Rows) != 1 {
		t.Fatal("the warning refused the write; it must not")
	}
}

// --- fixtures ---------------------------------------------------------------

func archiveENG(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.WriteDocument(t.Context(), "op-archive",
		tracker.ProjectSubject("ENG"), "", tracker.Project{
			V: 1, Key: "ENG", Name: "Engineering", Archived: true,
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("archive the project: %v", err)
	}
	r.drain()
}

func requireAField(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.WriteDocument(t.Context(), "op-policy",
		tracker.ProjectSubject("ENG"), "", tracker.Project{
			V: 1, Key: "ENG", Name: "Engineering",
			Fields: []tracker.FieldDef{{
				ID: "f-1", Slug: "impact", Name: "Impact",
				Type: tracker.FieldText, Required: true,
			}},
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("declare the field: %v", err)
	}
	r.drain()
}

func counterOf(t *testing.T, r *roundTrip, project string) int {
	t.Helper()
	var last int
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		err := tx.QueryRowContext(t.Context(),
			`SELECT last FROM tracker_counters WHERE project_key = ?`,
			project).Scan(&last)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}); err != nil {
		t.Fatalf("read %s's counter: %v", project, err)
	}
	return last
}

func taskOf(t *testing.T, r *roundTrip, id string) tracker.Task {
	t.Helper()
	var task tracker.Task
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var body []byte
		if err := tx.QueryRowContext(t.Context(),
			`SELECT document FROM tracker_tasks WHERE id = ?`, id).
			Scan(&body); err != nil {
			return err
		}
		return json.Unmarshal(body, &task)
	}); err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	return task
}

// ONE BULK EDIT APPLIES AT A TIME, ON ONE NODE AS ACROSS TWO — AND ONLY AN
// ADMITTED ONE IS COUNTED.
//
// The admission's lease is taken for the NODE, and a lease asked for again by
// the owner that holds it is renewed rather than refused — so on the lease
// alone every bulk edit a node's seats made would be admitted whatever else
// that node was applying, which on a single node is every bulk edit there is.
// The second is refused while the first applies, and admitted once it has.
//
// The occupancy counter is the fleet's read-degradation budget, so a refused
// bulk, which applies nothing, adds nothing to it.
func TestOneBulkEditAppliesAtATimeOnOneNode(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		filedTask(t, r, id)
	}
	var second error
	hooked.arm(func(_, opID string) bool { return opID == "op-bulk.t/t-1" },
		func() {
			_, second = r.writer.As("bea", tracker.AuthorAgent, tracker.Provenance{}).
				UpdateTasks(t.Context(), "op-bulk-2", []string{"t-3"}, "ENG",
					tracker.TaskPatch{Title: ptr("second")}, tracker.ChangeFields, nil)
		})

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTasks(t.Context(), "op-bulk", []string{"t-1", "t-2"},
		"ENG", tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if !hooked.didFire() {
		t.Fatal("the second bulk was never attempted, so this case is not the " +
			"shape it names")
	}
	if !errors.Is(second, tracker.ErrBulkInFlight) {
		t.Fatalf("a second bulk while the first applied answered %v, want it "+
			"refused as in flight", second)
	}
	// TWO SUBJECTS, at the one-record-a-second floor a writer with no
	// measured drain projects from: the admitted bulk's two seconds, and
	// nothing for the one refused.
	if got := bulkOccupancy(r); got != 2 {
		t.Errorf("the occupancy counter reads %v seconds, want the admitted "+
			"bulk's 2", got)
	}
	// AND THE CLAIM WAS GIVEN BACK: the next bulk here is admitted.
	if _, err := r.writer.UpdateTasks(t.Context(), "op-bulk-3", []string{"t-3"},
		"ENG", tracker.TaskPatch{Title: ptr("third")}, tracker.ChangeFields, nil); err != nil {
		t.Fatalf("a bulk after the first had applied answered %v", err)
	}
}

// A BULK EDIT'S OCCUPANCY IS PROJECTED FROM THE APPLIER'S MEASURED DRAIN, and
// the fraction of a second it comes to is kept.
//
// The projection is the bulk's records — one per task — over the drain in
// records a second, and it is what the fleet's read-degradation budget sums.
// Two tasks at four records a second is half a second: a bulk smaller than one
// second's drain, whose projection a whole-number path would lose entirely.
// [TestOneBulkEditAppliesAtATimeOnOneNode] pins the floor an unmeasured drain
// projects from; this pins the measured path, end to end from the call to the
// recorder.
//
// Mutation: make [tracker.Writer]'s drain ignore the measured rate and the
// occupancy reads the floor's 2 s; hand the projection to the recorder as a
// whole number and it reads 0.
func TestABulkEditProjectsItsOccupancyFromTheMeasuredDrain(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		filedTask(t, r, id)
	}
	writer, err := tracker.NewWriter(tracker.WriterDeps{
		Publisher: r.publisher, DB: r.db, NodeID: "node-a", Claims: r.claims,
		Metrics: r.metrics,
		// FOUR RECORDS A SECOND, as the applier would report having
		// measured it.
		Drain: func() float64 { return 4 },
		Actor: "ana", ActorKind: tracker.AuthorHuman,
		Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	done := tracker.StatusDone
	result, err := writer.UpdateTasks(t.Context(), "op-bulk", []string{"t-1", "t-2"},
		"ENG", tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
	if err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if len(result.Applied) != 2 {
		t.Fatalf("the bulk applied %v, want both tasks — a bulk that did not "+
			"run is not the path this case is about", result.Applied)
	}
	if got := bulkOccupancy(r); got != 0.5 {
		t.Errorf("the occupancy counter reads %v seconds, want 0.5: two records "+
			"over a measured four a second", got)
	}
}

// A BULK'S LEASE IS A WALK'S, HEARTBEATED: A HOLDER THAT DIES STOPS BLOCKING
// THE COMPANY'S BULKS WITHIN [tracker.ClaimTTL], HOWEVER LONG ITS BULK WAS
// PROJECTED TO TAKE — AND A CALLER REFUSED WHILE IT RUNS IS TOLD THE
// PROJECTION.
//
// At a hundredth of a record a second two tasks project to 200 s. A lease sized
// from that projection would hold a dead holder's claim for as long, refusing
// every peer's bulk while nothing applies; a heartbeated lease is renewed only
// while its holder runs, so it lapses one claim TTL after the last renewal.
// The projection still answers the question a refused caller has, so it rides
// the lease rather than sizing it.
//
// Mutation: size the lease from the projection and the claim is still held a
// claim TTL after its holder stopped renewing; leave the projection off the
// lease and the refused caller is told the lease's minute rather than the
// bulk's 200 s.
func TestABulkLeaseOutlivesADeadHolderByNoMoreThanTheClaimTTL(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		filedTask(t, r, id)
	}
	// THE STORE'S CLOCK IS THE CASE'S, so a lease outlasts its TTL without
	// the case sleeping through it.
	claims := memory.New()
	writerOn := func(node string) *tracker.Writer {
		t.Helper()
		w, err := tracker.NewWriter(tracker.WriterDeps{
			Publisher: r.publisher, DB: r.db, NodeID: node, Claims: claims,
			Drain: func() float64 { return 0.01 },
			Actor: "ana", ActorKind: tracker.AuthorHuman,
			Now: func() time.Time { return r.at },
		})
		if err != nil {
			t.Fatalf("NewWriter on %s: %v", node, err)
		}
		return w
	}
	holder, peer := writerOn("node-a"), writerOn("node-b")

	var refused error
	var held []coord.Lease
	var lapsed *coord.Lease
	hooked.arm(func(_, opID string) bool { return opID == "op-bulk.t/t-1" }, func() {
		ctx := context.WithoutCancel(t.Context())
		_, refused = peer.UpdateTasks(ctx, "op-peer", []string{"t-3"}, "ENG",
			tracker.TaskPatch{Title: ptr("peer")}, tracker.ChangeFields, nil)
		held, _ = claims.ListLive(ctx, coord.Class("bulk"))
		// THE HOLDER STOPS RENEWING, as a node that died does, and one
		// claim TTL passes on the store's clock.
		claims.Advance(tracker.ClaimTTL + time.Second)
		for _, lease := range held {
			lapsed, _ = claims.TryAcquire(ctx, lease.Resource, coord.AcquireOptions{
				Owner: "node-b", TTL: tracker.ClaimTTL,
			})
		}
	})

	done := tracker.StatusDone
	if _, err := holder.UpdateTasks(t.Context(), "op-bulk", []string{"t-1", "t-2"},
		"ENG", tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if !hooked.didFire() || len(held) != 1 {
		t.Fatalf("mid-bulk the store held %d bulk lease(s) (hook fired: %v), so "+
			"this case is not the shape it names", len(held), hooked.didFire())
	}
	if !errors.Is(refused, tracker.ErrBulkInFlight) {
		t.Fatalf("a peer's bulk while the first applied answered %v, want it "+
			"refused as in flight", refused)
	}
	var wait int
	if _, err := fmt.Sscanf(refused.Error()[strings.Index(refused.Error(), "retry in about "):],
		"retry in about %d seconds", &wait); err != nil {
		t.Fatalf("the refusal names no wait: %v (%v)", refused, err)
	}
	if wait < 190 || wait > 200 {
		t.Errorf("the refused caller was told to wait %d seconds, want the bulk's "+
			"projected 200 — two tasks at a hundredth of a record a second", wait)
	}
	if lapsed == nil {
		t.Errorf("a claim TTL after its holder stopped renewing, the bulk lease "+
			"(expiring %s) still refused a peer — a dead holder is blocking the "+
			"company's bulks", held[0].ExpiresAt)
	}
}

// A BULK THAT RUNS PAST A HEARTBEAT KEEPS ITS LEASE.
//
// The other half of [TestABulkLeaseOutlivesADeadHolderByNoMoreThanTheClaimTTL]:
// a lease that lapses within a claim TTL of its holder dying must not lapse
// under a holder that is alive and slow, or a second bulk is admitted beside
// the first. The holder renews every [tracker.ClaimHeartbeat] for as long as
// the bulk runs, so this case holds one bulk open across a heartbeat — on the
// real clock, because the heartbeat is a real ticker — and reads the lease's
// expiry either side of it.
//
// Mutation: take the bulk's lease without its heartbeat and the expiry a
// heartbeat later is the one it was acquired with.
func TestABulkThatRunsPastAHeartbeatKeepsItsLease(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		filedTask(t, r, id)
	}
	claims := memory.New()
	holder, err := tracker.NewWriter(tracker.WriterDeps{
		Publisher: r.publisher, DB: r.db, NodeID: "node-a", Claims: claims,
		Actor: "ana", ActorKind: tracker.AuthorHuman,
		Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	var before, after []coord.Lease
	hooked.arm(func(_, opID string) bool { return opID == "op-bulk.t/t-1" }, func() {
		ctx := context.WithoutCancel(t.Context())
		before, _ = claims.ListLive(ctx, coord.Class("bulk"))
		time.Sleep(tracker.ClaimHeartbeat + 2*time.Second)
		after, _ = claims.ListLive(ctx, coord.Class("bulk"))
	})
	done := tracker.StatusDone
	if _, err := holder.UpdateTasks(t.Context(), "op-bulk", []string{"t-1", "t-2"},
		"ENG", tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("the store held %d bulk lease(s) before the heartbeat and %d "+
			"after, want one throughout", len(before), len(after))
	}
	if !after[0].ExpiresAt.After(before[0].ExpiresAt) {
		t.Errorf("a heartbeat into the bulk its lease still expires at %s, as it "+
			"was acquired — the holder is not renewing it", after[0].ExpiresAt)
	}
}

// bulkOccupancy is the projected applier seconds this rig's bulk edits were
// admitted for.
func bulkOccupancy(r *roundTrip) float64 {
	var total float64
	for _, snap := range r.metrics.Read() {
		if snap.Name == metrics.TrackerBulkApplySeconds {
			total += snap.Total
		}
	}
	return total
}

// WITH THE STORE UNREACHABLE A BULK IS ADMITTED — BUT STILL ONE AT A TIME HERE.
//
// The admission fails OPEN on an unknown, because the log is correct with two
// bulks in flight and merely slow. What the store cannot say is whether a PEER
// is applying one; whether this node is, it knows without asking. So a second
// bulk on this node is refused while the first applies, and the first hands
// its claim back like any other.
func TestABulkEditOnAnUnreachableStoreIsStillOneAtATimeHere(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		filedTask(t, r, id)
	}
	var second error
	hooked.arm(func(_, opID string) bool { return opID == "op-bulk.t/t-1" },
		func() {
			_, second = r.writer.UpdateTasks(t.Context(), "op-bulk-2",
				[]string{"t-3"}, "ENG", tracker.TaskPatch{Title: ptr("second")},
				tracker.ChangeFields, nil)
		})

	r.claims.Break(nil)
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTasks(t.Context(), "op-bulk", []string{"t-1", "t-2"},
		"ENG", tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("a bulk with the store unreachable answered %v, want it "+
			"admitted", err)
	}
	if !hooked.didFire() {
		t.Fatal("the second bulk was never attempted, so this case is not the " +
			"shape it names")
	}
	if !errors.Is(second, tracker.ErrBulkInFlight) {
		t.Fatalf("a second bulk on this node answered %v, want it refused as "+
			"in flight", second)
	}
	if _, err := r.writer.UpdateTasks(t.Context(), "op-bulk-3", []string{"t-3"},
		"ENG", tracker.TaskPatch{Title: ptr("third")}, tracker.ChangeFields, nil); err != nil {
		t.Fatalf("a bulk after the first had applied answered %v", err)
	}
}

// AND A BULK'S CLAIM IS NOT FREE HERE UNTIL ITS LEASE IS GIVEN BACK, for the
// reason a walk's is not ([TestAClaimIsNotFreeHereUntilItsLeaseIsGivenBack]).
func TestABulkClaimIsNotFreeHereUntilItsLeaseIsGivenBack(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		filedTask(t, r, id)
	}
	claims := &releasingClaims{Claims: r.claims}
	writer, err := tracker.NewWriter(tracker.WriterDeps{
		Publisher: r.publisher, DB: r.db, NodeID: "node-a", Claims: claims,
		Actor: "ana", ActorKind: tracker.AuthorHuman,
		Now: func() time.Time { return r.at },
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	var second error
	claims.arm(func() {
		_, second = writer.As("bea", tracker.AuthorAgent, tracker.Provenance{}).
			UpdateTasks(t.Context(), "op-bulk-2", []string{"t-2"}, "ENG",
				tracker.TaskPatch{Title: ptr("second")}, tracker.ChangeFields, nil)
	})
	if _, err := writer.UpdateTasks(t.Context(), "op-bulk", []string{"t-1"}, "ENG",
		tracker.TaskPatch{Title: ptr("first")}, tracker.ChangeFields, nil); err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if !claims.fired() {
		t.Fatal("nothing asked for the claim while it was being given back, so " +
			"this case is not the shape it names")
	}
	if !errors.Is(second, tracker.ErrBulkInFlight) {
		t.Fatalf("a bulk asking while the admission's lease was being given "+
			"back answered %v, want it refused as in flight", second)
	}
}

// A CREATE RETRIED AFTER ITS KEY MINT LANDED KEYS THE TASK FROM THE NUMBER THAT
// MINT TOOK.
//
// The retry's mint is answered from this node's operation ledger, so nothing
// decides it again — and the number it minted is read back from the counter
// record that landed. A number taken from the retry's own decision would be
// the counter AFTER the first mint: the task would carry a key nobody minted,
// and the next create would mint the same one.
//
// Mutation: take the number from the mint's decide closure and the retry, which
// ran no decide, derives key ENG-0; take it from a decide the retry runs again
// — the publisher without its ledger answer — and the task is keyed ENG-2, a
// number nobody minted.
func TestARetriedCreateKeysFromTheNumberItsFirstMintTook(t *testing.T) {
	t.Parallel()
	task := newTask("t-1")
	task.Key = ""
	broker := &refusingAppender{subject: tracker.TaskSubject(task.ID).Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()

	// THE FIRST ATTEMPT: the mint lands and is applied here, and the task's
	// own append is refused, so the caller tries the create again under the
	// same operation id.
	broker.refusing(true)
	if _, err := r.writer.CreateTask(t.Context(), "op-create", task, nil); err == nil {
		t.Fatal("the first attempt filed the task the broker refused, so this " +
			"case is not the shape it names")
	}
	r.drain()
	if got := counterOf(t, r, "ENG"); got != 1 {
		t.Fatalf("ENG's counter is %d after the first attempt's mint, want 1", got)
	}
	broker.refusing(false)

	retried, err := r.writer.CreateTask(t.Context(), "op-create", task, nil)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if retried.Key != "ENG-1" {
		t.Fatalf("the retried create keyed its task %q, want ENG-1 — the number "+
			"its first mint took", retried.Key)
	}
	r.drain()
	if got := counterOf(t, r, "ENG"); got != 1 {
		t.Errorf("ENG's counter is %d after the retry, want 1 — a retry of a "+
			"mint that landed mints nothing", got)
	}
	if got := oneTask(t, r, task.ID).Key; got != "ENG-1" {
		t.Errorf("the task reads back keyed %q, want ENG-1", got)
	}

	// AND THE NEXT CREATE TAKES THE NEXT NUMBER, rather than the one the
	// retry would have claimed.
	next := newTask("t-2")
	next.Key = ""
	created, err := r.writer.CreateTask(t.Context(), "op-next", next, nil)
	if err != nil {
		t.Fatalf("the next create: %v", err)
	}
	if created.Key != "ENG-2" {
		t.Errorf("the next create keyed its task %q, want ENG-2", created.Key)
	}
}

// A RETRIED CREATE CARRIES THE FIELDS ITS OWN APPEND COERCED.
//
// The retry's mint is answered from this node's operation ledger, which runs no
// decide — so anything the create took out of the mint's closure is the zero it
// started as, and the task would be published with its fields as the caller
// typed them: "High" stored where every filter, grouping and total looks for
// "o-high". The task's own append decides what it carries.
//
// Mutation: take the coerced fields out of the mint's guard again and the
// retried task stores "High".
func TestARetriedCreateCarriesTheFieldsItsOwnAppendCoerced(t *testing.T) {
	t.Parallel()
	task := newTask("t-1")
	task.Key = ""
	task.Fields = map[string]json.RawMessage{"f-impact": json.RawMessage(`"High"`)}
	broker := &refusingAppender{subject: tracker.TaskSubject(task.ID).Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	seedFields(t, r)

	broker.refusing(true)
	if _, err := r.writer.CreateTask(t.Context(), "op-create", task, nil); err == nil {
		t.Fatal("the first attempt filed the task the broker refused, so this " +
			"case is not the shape it names")
	}
	r.drain()
	broker.refusing(false)

	if _, err := r.writer.CreateTask(t.Context(), "op-create", task, nil); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	r.drain()
	if got := string(taskOf(t, r, task.ID).Fields["f-impact"]); got != `"o-high"` {
		t.Fatalf("the retried task stores f-impact as %s, want \"o-high\" — the "+
			"option's id, which every filter over the field compares against", got)
	}
}

// A RETRIED PROMOTION MARKS THE PARENT ITS OWN APPEND READ.
//
// The retry's mint is answered from the ledger, so a parent carried out of the
// mint's closure is an empty task: its project is blank, and the parent's own
// append is refused as naming none — on every retry, so the item is never
// marked. The parent's append reads the parent in its own snapshot instead.
//
// Mutation: build the parent's checklist from a parent read in the mint's guard
// and the retry is refused, the item unmarked.
func TestARetriedPromotionMarksTheParentItsOwnAppendRead(t *testing.T) {
	t.Parallel()
	broker := &refusingAppender{subject: tracker.TaskSubject("t-2").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	parent := newTask("t-1")
	parent.Key = ""
	parent.Checklists = []tracker.Checklist{{
		ID: "l-1", Name: "steps",
		Items: []tracker.ChecklistItem{{ID: "i-1", Name: "wire it"}},
	}}
	if _, err := r.writer.CreateTask(t.Context(), "op-1", parent, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	subtask := newTask("t-2")
	subtask.Key = ""
	broker.refusing(true)
	if _, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1", subtask,
		nil); err == nil {
		t.Fatal("the first attempt created the subtask the broker refused, so " +
			"this case is not the shape it names")
	}
	r.drain()
	broker.refusing(false)

	if _, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1", subtask,
		nil); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	r.drain()
	item := taskOf(t, r, "t-1").Checklists[0].Items[0]
	if item.PromotedTo == nil || *item.PromotedTo != "t-2" {
		t.Fatalf("after the retry the item points at %v, want the subtask t-2",
			item.PromotedTo)
	}
	if child := taskOf(t, r, "t-2"); child.Parent == nil || *child.Parent != "t-1" {
		t.Fatalf("the subtask's parent is %v, want t-1", child.Parent)
	}
}

// A BULK EDIT RE-RUN OVER ITS FAILURES WRITES THEM.
//
// The caller re-runs the tasks that failed under the same op id, which is a
// shorter list than the first run's. A step named by its place in the list maps
// the re-run's first task onto the first run's first — whose ledger row answers
// `applied` for a task the re-run never wrote.
//
// Mutation: name each step by its index again and the re-run reports t-2
// applied while it stays todo.
func TestABulkEditRerunOverItsFailuresWritesThem(t *testing.T) {
	t.Parallel()
	broker := &refusingAppender{subject: tracker.TaskSubject("t-2").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	for _, id := range []string{"t-1", "t-2"} {
		task := newTask(id)
		task.Key = ""
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}

	done := tracker.StatusDone
	broker.refusing(true)
	first, err := r.writer.UpdateTasks(t.Context(), "op-bulk",
		[]string{"t-1", "t-2"}, "ENG", tracker.TaskPatch{Status: &done},
		tracker.ChangeStatus, nil)
	if err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if _, failed := first.Failed["t-2"]; !failed {
		t.Fatalf("the first run failed %v, and this case needs t-2 among them",
			first.Failed)
	}
	r.drain()
	broker.refusing(false)

	rerun, err := r.writer.UpdateTasks(t.Context(), "op-bulk", []string{"t-2"},
		"ENG", tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
	if err != nil {
		t.Fatalf("the re-run: %v", err)
	}
	if len(rerun.Applied) != 1 || rerun.Applied[0] != "t-2" {
		t.Fatalf("the re-run applied %v, want t-2", rerun.Applied)
	}
	r.drain()
	if got := taskOf(t, r, "t-2").Status; got != tracker.StatusDone {
		t.Fatalf("t-2 is %s after a re-run that reported it applied — the step "+
			"was answered from another task's ledger row", got)
	}
}

// AN EDIT ADDRESSED TO A PROJECT THE TASK IS NOT IN IS REFUSED.
//
// The record's scope is built from the project the caller names, and a task's
// path in the scope alphabet sits under its project — so a record addressed to
// the wrong one is filed, where a node defers it, somewhere no later write to
// the task probes. A caller naming the wrong project read the task before it
// moved, and re-reads.
//
// Mutation: drop the check and the edit lands, its record claiming a task in
// OPS.
func TestAnEditAddressedToAnotherProjectIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-1")
	task.Key = ""
	if _, err := r.writer.CreateTask(t.Context(), "op-1", task, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	done := tracker.StatusDone
	_, err := r.writer.UpdateTask(t.Context(), "op-edit", "t-1", "OPS",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
	if !errors.Is(err, tracker.ErrStaleVersion) {
		t.Fatalf("an edit addressed to OPS for a task in ENG = %v, want a "+
			"refusal that sends the caller to re-read", err)
	}
	r.drain()
	if got := taskOf(t, r, "t-1").Status; got == tracker.StatusDone {
		t.Fatal("the edit addressed to the wrong project landed")
	}
}

// refusingAppender is a broker that refuses every append on one subject with a
// decision of its own — nothing stored, and a reason the write path answers
// at once rather than retries.
type refusingAppender struct {
	statelog.Appender
	subject string

	mu      sync.Mutex
	refuses bool
}

func (a *refusingAppender) refusing(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refuses = on
}

func (a *refusingAppender) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	a.mu.Lock()
	refuses := a.refuses && subject == a.subject
	a.mu.Unlock()
	if refuses {
		return 0, false, &jetstream.APIError{Code: 400, ErrorCode: 10999,
			Description: "refused for this case"}
	}
	return a.Appender.Append(ctx, subject, msgID, expect, body)
}

// A MOVE RETRIED AFTER ITS SUBTREE GREW MINTS A RANGE FOR THE SUBTREE IT HAS.
//
// The key mint recovers its range from the counter record it landed, which
// says where the counter ended and not how many numbers the mint took — so a
// range recovered for a count other than the one that minted it starts at a
// number an earlier mint already handed out. The move's count is therefore
// part of its mint's operation id: a retry that needs more numbers than its
// first attempt took mints them afresh, and the first attempt's range is a
// gap.
//
// Mutation: drop the count from the move's mint step and the retry keys the
// root with a number the target project's own tasks already hold.
func TestAMoveRetriedAfterItsSubtreeGrewMintsAFreshRange(t *testing.T) {
	t.Parallel()
	root := tracker.TaskSubject("m-root").Wire()
	broker := &refusingAppender{subject: root}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
		tracker.ProjectSubject("OPS"), "", tracker.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed the target project: %v", err)
	}
	r.drain()
	// FIVE TASKS ALREADY IN THE TARGET, so a range recovered too low lands
	// on keys that are taken.
	for i := 1; i <= 5; i++ {
		seeded := newTask(fmt.Sprintf("ops-%d", i))
		seeded.Key, seeded.Project = "", "OPS"
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-ops-%d", i),
			seeded, nil); err != nil {
			t.Fatalf("seed OPS-%d: %v", i, err)
		}
		r.drain()
	}
	subtask := func(id string) {
		t.Helper()
		kid := newTask(id)
		kid.Key = ""
		parent := "m-root"
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	moving := newTask("m-root")
	moving.Key = ""
	if _, err := r.writer.CreateTask(t.Context(), "op-root", moving, nil); err != nil {
		t.Fatalf("CreateTask m-root: %v", err)
	}
	r.drain()
	subtask("m-kid-1")

	// THE FIRST ATTEMPT mints two numbers — the root and its one subtask —
	// and is refused at the root's own append.
	broker.refusing(true)
	if _, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "m-root",
		"OPS", nil); err == nil {
		t.Fatal("the first attempt moved the root the broker refused, so this " +
			"case is not the shape it names")
	}
	r.drain()
	if got := counterOf(t, r, "OPS"); got != 7 {
		t.Fatalf("OPS's counter is %d after the first attempt, want 7 — five "+
			"seeded and the two the move minted", got)
	}

	// THE SUBTREE GROWS, and the retry needs three numbers.
	broker.refusing(false)
	subtask("m-kid-2")
	if _, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "m-root",
		"OPS", nil); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	r.drain()

	if got := oneTask(t, r, "m-root").Key; got != "OPS-8" {
		t.Fatalf("the moved root is keyed %q, want OPS-8 — the first number of a "+
			"fresh three-number range after the seven OPS had handed out", got)
	}
	keys := r.strings(`SELECT key FROM tracker_tasks WHERE project_key = 'OPS' ORDER BY key`)
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			t.Errorf("two tasks in OPS are keyed %s", key)
		}
		seen[key] = true
	}
}
