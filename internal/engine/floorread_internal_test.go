package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// listingsHeld is a coordination store whose every LISTING of the positions
// register — a key class read whole, or watched — is held from the moment it
// is armed until the case lets it go, while every read of one key answers at
// once. Which methods list is not this file's opinion:
// [TestListingsHeldHoldsEveryListingOfTheRegister] reads them off the KV
// backend's own source.
//
// That is the shape of a loaded clustered broker, measured: a listing there is
// a key index read plus an ordered consumer created and deleted on the
// register's stream, and the create and the delete are proposals to the
// metadata group every stream and consumer in the fleet is placed through, so
// under load a listing runs for seconds — past the client's five-second API
// timeout on the worst of them — while a key read is one request to the
// stream's leader and answers in milliseconds. Held rather than slowed, so a
// path that waits on a listing waits for the case's whole bound and cannot
// pass by being quick enough.
type listingsHeld struct {
	wrappedFleet

	armed   atomic.Bool
	release chan struct{}
	once    sync.Once
	listed  atomic.Int64

	mu    sync.Mutex
	asked []string
	// byWrite is every domain a floor read named under a context a case
	// marked ([markedWrite]), by the mark: the reads ONE write made,
	// told apart from the node's own loops, which read floors too.
	byWrite map[string][]string
}

func newListingsHeld(register coord.Fleet) *listingsHeld {
	return &listingsHeld{wrappedFleet: register, release: make(chan struct{}),
		byWrite: map[string][]string{}}
}

// writeMark is the context key a case marks one write's context with.
type writeMark struct{}

// markedWrite is ctx marked as the write named name: every floor read made
// under it is recorded as that write's ([listingsHeld.floorsOfWrite]).
func markedWrite(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, writeMark{}, name)
}

// floorsOfWrite is every domain a floor read named under the write's mark.
func (f *listingsHeld) floorsOfWrite(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.byWrite[name])
}

// letGo releases every listing held and every one after it.
func (f *listingsHeld) letGo() { f.once.Do(func() { close(f.release) }) }

// hold is every listing's wait: nothing until armed, then until the case lets
// go or the caller's context ends.
func (f *listingsHeld) hold(ctx context.Context) error {
	if !f.armed.Load() {
		return nil
	}
	f.listed.Add(1)
	select {
	case <-f.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// floorsAsked is every domain a floor read named, in order.
func (f *listingsHeld) floorsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

func (f *listingsHeld) Floor(ctx context.Context, domain string) (coord.TrimFloor, bool, error) {
	f.mu.Lock()
	f.asked = append(f.asked, domain)
	if name, ok := ctx.Value(writeMark{}).(string); ok {
		f.byWrite[name] = append(f.byWrite[name], domain)
	}
	f.mu.Unlock()
	return f.wrappedFleet.Floor(ctx, domain)
}

func (f *listingsHeld) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	if err := f.hold(ctx); err != nil {
		return nil, err
	}
	return f.wrappedFleet.Positions(ctx)
}

func (f *listingsHeld) Holds(ctx context.Context) ([]coord.TrimHold, error) {
	if err := f.hold(ctx); err != nil {
		return nil, err
	}
	return f.wrappedFleet.Holds(ctx)
}

func (f *listingsHeld) BackupPoints(ctx context.Context) ([]coord.BackupPoint, error) {
	if err := f.hold(ctx); err != nil {
		return nil, err
	}
	return f.wrappedFleet.BackupPoints(ctx)
}

func (f *listingsHeld) Admissions(ctx context.Context) ([]coord.Admission, error) {
	if err := f.hold(ctx); err != nil {
		return nil, err
	}
	return f.wrappedFleet.Admissions(ctx)
}

func (f *listingsHeld) MaintenanceAcks(ctx context.Context, stream string) ([]coord.MaintenanceAck, error) {
	if err := f.hold(ctx); err != nil {
		return nil, err
	}
	return f.wrappedFleet.MaintenanceAcks(ctx, stream)
}

func (f *listingsHeld) ListSeatPauses(ctx context.Context) ([]coord.SeatPause, error) {
	if err := f.hold(ctx); err != nil {
		return nil, err
	}
	return f.wrappedFleet.ListSeatPauses(ctx)
}

func (f *listingsHeld) WatchSeatPauses(ctx context.Context) (<-chan coord.SeatPauseUpdate, error) {
	if err := f.hold(ctx); err != nil {
		return nil, err
	}
	return f.wrappedFleet.WatchSeatPauses(ctx)
}

// ForgetBackupPoint lists the register because it walks the owner's backup
// points under their key class to purge them, and SeatAccessRecords because it
// reads an integration's whole ledger: both are listings the KV backend makes,
// held like the rest.
func (f *listingsHeld) ForgetBackupPoint(ctx context.Context, owner string) error {
	if err := f.hold(ctx); err != nil {
		return err
	}
	return f.wrappedFleet.ForgetBackupPoint(ctx, owner)
}

func (f *listingsHeld) SeatAccessRecords(ctx context.Context, kind string) (map[string][]byte, error) {
	if err := f.hold(ctx); err != nil {
		return nil, err
	}
	return f.wrappedFleet.SeatAccessRecords(ctx, kind)
}

// listingBound is how long a case waits on a read the write path makes: the
// nats client's default API timeout, the bound a floor listing ran past on a
// loaded three-member fleet and refused a page create `floor_unknown` with
// the register perfectly readable.
const listingBound = 5 * time.Second

// A WRITE FENCE READS ITS OWN DOMAIN'S FLOOR, BY ITS KEY, AND LISTS NOTHING.
//
// The fence at an expectation of zero must read the floor FRESH, after the
// write's own read of its subject — the floor theorem's order (statelog's
// package doc) — so it cannot be served from a memo or moved off the request
// path. What it can do is take the cheapest read that order allows. It used to
// list every domain's floor to answer for one, and a listing is a consumer
// created and deleted through the metadata group: every write at zero then
// waited on two metadata proposals, and on a loaded fleet a page create was
// refused `floor_unknown` when the consumer's create ran past the client's
// API timeout. With every listing of the register held, the fence still
// answers, the floor it answers is its own domain's, and the one read it made
// named that domain alone.
func TestAWriteFenceReadsItsFloorWithoutListingTheRegister(t *testing.T) {
	t.Parallel()
	fleet := newListingsHeld(coordmem.NewFleet())
	t.Cleanup(fleet.letGo)
	const generation = 2
	for domain, floor := range map[string]uint64{
		worklet.Domain{}.Name(): 700,
		doclet.Domain{}.Name():  300,
	} {
		if err := fleet.PutFloor(t.Context(), coord.TrimFloor{
			Domain: domain, Generation: generation, TrimTo: floor, Floor: floor,
			At: time.Now().UTC(), By: "peer",
		}); err != nil {
			t.Fatalf("publish %s's floor: %v", domain, err)
		}
	}
	fleet.armed.Store(true)
	s := &stateLog{fleet: fleet}

	ctx, cancel := context.WithTimeout(t.Context(), listingBound)
	defer cancel()
	got, err := s.floorOf(worklet.Domain{}.Name())(ctx, generation)
	if err != nil {
		t.Fatalf("the tracker's write fence could not read its floor with the "+
			"register's listings held: %v — every write at an expectation of "+
			"zero waits on whatever this read waits on", err)
	}
	if got != 700 {
		t.Fatalf("the tracker's fence read a floor of %d, want its own 700", got)
	}
	if n := fleet.listed.Load(); n != 0 {
		t.Fatalf("the fence's floor read made %d listing(s) of the register, want "+
			"none: a listing is a consumer created through the metadata group", n)
	}
	if asked := fleet.floorsAsked(); !slices.Equal(asked, []string{worklet.Domain{}.Name()}) {
		t.Fatalf("the fence's floor read asked about %v, want the tracker alone — "+
			"every other domain's row is a request this write did not need", asked)
	}
}

// AND A PAGE CREATE AND A TRACKER WRITE AT AN EXPECTATION OF ZERO LAND WHILE
// EVERY LISTING OF THE REGISTER IS HELD, through the engine's own wiring: the
// incident this replaces was exactly this write, refused while the register
// answered every key read in milliseconds. The node's own loops that do list —
// the heartbeat, the readiness verdict, the trim — wait behind the hold, and
// no write may wait with them.
func TestAWriteAtZeroLandsWhileTheRegistersListingsAreHeld(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	fleet := newListingsHeld(back.Fleet)
	back.Fleet = fleet
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	// LET GO BEFORE THE STOP, which waits for the loops the hold parked:
	// cleanups run last-registered first.
	t.Cleanup(fleet.letGo)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	native := e.native.Load()
	if native.writer == nil || native.pages == nil {
		t.Fatal("the node runs no tracker writer or no page store")
	}

	fleet.armed.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), listingBound)
	defer cancel()
	// EACH WRITE'S CONTEXT IS MARKED, so the floor reads asserted below are
	// that write's own fence's — never the heartbeat's or the readiness
	// verdict's, which read every domain's floor by its key on their own
	// clocks and would answer for a fence that read nothing.
	if _, _, err := native.pages.EnsureContainer(markedWrite(ctx, "page create"),
		testActivation, "HELD", "Held", ""); err != nil {
		t.Fatalf("a page create at an expectation of zero was refused with every "+
			"listing of the register held: %v", err)
	}
	res, err := native.writer.EvictNode(markedWrite(ctx, "tracker write"), "op-held", "node-held")
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a tracker write at an expectation of zero answered %+v, %v with "+
			"every listing of the register held, want applied", res, err)
	}
	for write, domain := range map[string]string{
		"page create":   doclet.Domain{}.Name(),
		"tracker write": worklet.Domain{}.Name(),
	} {
		asked := fleet.floorsOfWrite(write)
		if len(asked) == 0 {
			t.Errorf("the %s read no floor: a write at an expectation of zero that "+
				"reads no floor clears a fence the trim may have moved past", write)
		}
		for _, got := range asked {
			if got != domain {
				t.Errorf("the %s's fence read %s's floor, want its own domain's (%s) "+
					"alone: %v", write, got, domain, asked)
			}
		}
	}
}

// AND THE HOLD COVERS EVERY LISTING THERE IS. The case above proves a write
// waits on no listing only for the listings [listingsHeld] holds, so a listing
// of the register it forgot — a key class read whole, or watched — is one a
// write could wait on and still pass. The set is read off the KV backend's own
// source: every kv.FleetStore method that walks a class of the positions
// bucket (eachPositionKey, or eachUnder, watchUnder or watchRecords over
// f.positions), or calls one that does, must be a method listingsHeld holds.
func TestListingsHeldHoldsEveryListingOfTheRegister(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	listing := registerListings(t, filepath.Join(root, "internal", "coord", "kv"))
	// THE CONTROL: a reading of the backend that found none of the
	// listings it plainly has would pass any wrapper.
	for _, known := range []string{"Positions", "Holds", "ListSeatPauses", "WatchSeatPauses"} {
		if !slices.Contains(listing, known) {
			t.Fatalf("the KV backend's listings of the register read as %v, which "+
				"misses %s — the reading of the source is broken", listing, known)
		}
	}
	held := heldMethods(t, filepath.Join(root, "internal", "engine", "floorread_internal_test.go"))
	for _, method := range listing {
		if !slices.Contains(held, method) {
			t.Errorf("kv.FleetStore.%s lists the positions register and listingsHeld "+
				"does not hold it: a write that waits on it passes the case above", method)
		}
	}
}

// registerListings is every exported FleetStore method in dir's non-test
// files that lists a class of the positions bucket, directly or through
// another FleetStore method that does, sorted.
func registerListings(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	// calls is, per FleetStore method, every method name its body calls;
	// lists is those that walk the bucket themselves, then transitively.
	calls := map[string][]string{}
	lists := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || receiverType(fn) != "FleetStore" {
				continue
			}
			name := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "eachPositionKey" ||
						(fun.Sel.Name == "eachUnder" && namesPositions(call.Args)) {
						lists[name] = true
					}
					calls[name] = append(calls[name], fun.Sel.Name)
				case *ast.Ident:
					// watchRecords is the one watch of a record family,
					// which calls watchUnder over the bucket it is handed.
					if (fun.Name == "watchUnder" || fun.Name == "watchRecords") &&
						namesPositions(call.Args) {
						lists[name] = true
					}
				}
				return true
			})
		}
	}
	for changed := true; changed; {
		changed = false
		for name, callees := range calls {
			if lists[name] {
				continue
			}
			for _, callee := range callees {
				if lists[callee] {
					lists[name], changed = true, true
					break
				}
			}
		}
	}
	var out []string
	for name := range lists {
		// The walkers themselves are how a listing is made, not a
		// listing a caller asks the register for.
		if ast.IsExported(name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// namesPositions is whether any argument is the receiver's positions bucket.
func namesPositions(args []ast.Expr) bool {
	for _, arg := range args {
		if sel, ok := arg.(*ast.SelectorExpr); ok && sel.Sel.Name == "positions" {
			return true
		}
	}
	return false
}

// receiverType is the named type fn is a method of, through a pointer.
func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// heldMethods is every listingsHeld method in path whose body calls its hold.
func heldMethods(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var held []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || receiverType(fn) != "listingsHeld" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "hold" {
					held = append(held, fn.Name.Name)
					return false
				}
			}
			return true
		})
	}
	return held
}
