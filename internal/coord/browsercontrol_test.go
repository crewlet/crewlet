package coord_test

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// controlTake is a take of launch by a person of their own, shown as name.
func controlTake(launch uuid.UUID, name string) coord.BrowserControlTake {
	return coord.BrowserControlTake{
		Launch: launch, Turn: uuid.New(), Seat: uuid.New(),
		Owner: coord.BrowserControlOwner{
			Person: uuid.New(), Credential: iam.SessionName(uuid.NewString()), Take: uuid.New(),
		},
		Holder: coord.BrowserControlHolder{Seat: uuid.New(), Name: name},
	}
}

// A BROWSER CONTROL'S RECORD AT ITS WORST FITS WHAT A LEASE CARRIES. It rides
// the lease's Meta, re-sent on every renewal and refused past
// coord.MaxLeaseMetaBytes, so a record past it would refuse its own take. The
// worst is five names of U+2028, which encoding/json writes as six-byte
// escapes, every epoch at the int64 maximum, every instant to the nanosecond,
// the longest cause, four ends carried and the newest ended with its instant.
//
// Mutation: carry five ends (coord.MaxBrowserControlEnds = 5), and it does not.
func TestABrowserControlAtItsWorstFitsTheLeaseMetaBound(t *testing.T) {
	t.Parallel()
	widest := strings.Repeat(" ", coord.MaxBrowserControlHolderName)
	at := time.Date(2026, time.November, 19, 23, 59, 59, 123456789, time.UTC)
	was := coord.BrowserControlJudged{Epoch: math.MaxInt64, TakenAt: at, At: at}
	launch := uuid.New()
	var (
		meta map[string]any
		err  error
	)
	for i := range coord.MaxBrowserControlEnds + 1 {
		over := was
		if i == 0 {
			over = coord.BrowserControlJudged{}
		}
		if meta, err = coord.BrowserControlTakeMeta(controlTake(launch, widest), meta, over); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		if meta, err = coord.BrowserControlEndMeta(meta, coord.ControlHandedBack); err != nil {
			t.Fatalf("end %d: %v", i, err)
		}
	}
	if meta, err = coord.BrowserControlKeptEnd(meta, was); err != nil {
		t.Fatal(err)
	}
	control, err := coord.DecodeBrowserControl(launch, meta, was)
	switch {
	case err != nil:
		t.Fatal(err)
	case len(control.Earlier) != coord.MaxBrowserControlEnds:
		t.Fatalf("the worst record carries %d ends, want %d", len(control.Earlier), coord.MaxBrowserControlEnds)
	case control.Current == nil || control.Current.EndedBy != coord.ControlHandedBack:
		t.Fatalf("the worst record's newest tenure is %+v, want it ended", control.Current)
	}
	size, err := coord.LeaseMetaBytes(meta)
	if err != nil {
		t.Fatal(err)
	}
	if size > coord.MaxLeaseMetaBytes {
		t.Fatalf("a browser control's record at its worst is %d bytes, past the %d a lease carries: its take "+
			"would be refused", size, coord.MaxLeaseMetaBytes)
	}
	t.Logf("a browser control's record at its worst: %d of %d bytes", size, coord.MaxLeaseMetaBytes)
}

// EVERY PERSON'S DISPLAY NAME MAKES A HOLDER'S NAME A TAKE ACCEPTS, and one
// that already keeps the rule is kept as it is. The identity estate bounds a
// display name by its bytes alone (internal/iamdomain's MaxName), so a take
// built from one as it stands would fail, for as long as they kept it, for
// every person whose name runs long or holds a control character.
//
// Mutation: leave the control characters in, and those takes are refused.
func TestEveryDisplayNameMakesAHolderNameATakeAccepts(t *testing.T) {
	t.Parallel()
	const handle = "ada"
	for _, c := range []struct {
		what, display, fallback, want string
	}{
		{"a plain name", "Ada Lovelace", handle, "Ada Lovelace"},
		{"a name at the bound", nameAtTheBound, handle, nameAtTheBound},
		{"an ASCII name the identity estate keeps whole", strings.Repeat("a", 256), handle,
			strings.Repeat("a", coord.MaxBrowserControlHolderName)},
		{"a name one character past the bound", nameAtTheBound + "é", handle, nameAtTheBound},
		{"a name holding C0 controls", "Ada\tLovelace\r\n", handle, "Ada Lovelace  "},
		{"a name holding DEL and C1 controls", "Ada\x7fLove\u0085lace", handle, "Ada Love lace"},
		{"a name that is not UTF-8", "Ada \xff\xfeLovelace", handle, "Ada \uFFFDLovelace"},
		{"no name", "", handle, handle},
		{"a name of white space", " \u2028 ", handle, handle},
		{"a name of control characters", "\x00\x1b\u009f", handle, handle},
		{"a name blank as far as the bound", strings.Repeat(" ", coord.MaxBrowserControlHolderName) + "Ada", handle,
			handle},
		{"a fallback that breaks the rule", "", "ada\nl", "ada l"},
	} {
		got := coord.BrowserControlHolderName(c.display, c.fallback)
		if got != c.want {
			t.Errorf("%s: %q made the holder's name %q, want %q", c.what, c.display, got, c.want)
		}
		if err := controlTake(uuid.New(), got).Validate(); err != nil {
			t.Errorf("%s: a take shown as %q is refused: %v", c.what, got, err)
		}
	}
	// NOTHING TO SHOW AT ALL is a caller's mistake, which the take refuses.
	if got := coord.BrowserControlHolderName("\n", " "); got != "" {
		t.Errorf("a blank name and a blank fallback made the holder's name %q, want none", got)
	}
}

// nameAtTheBound is a name of coord.MaxBrowserControlHolderName characters,
// each two bytes.
var nameAtTheBound = strings.Repeat("é", coord.MaxBrowserControlHolderName)

// EVERY END IS ONE OF A CLOSED LIST, AND A LAPSE IS THE ONE NOBODY WRITES: the
// store reads a lapse from its own clock, so a node naming one would say a
// tenure ran out that the store still holds.
//
// Mutation: make a lapse written, and the end check passes it.
func TestEveryBrowserControlEndIsAClosedValue(t *testing.T) {
	t.Parallel()
	for _, e := range coord.BrowserControlEnds {
		if !e.Valid() {
			t.Errorf("%q is listed and not valid", e)
		}
		if written := e != coord.ControlLapsed; e.Written() != written ||
			(coord.CheckBrowserControlEnd(e) == nil) != written {
			t.Errorf("%q written = %v, want %v", e, e.Written(), written)
		}
	}
	for _, e := range []coord.BrowserControlEnd{"", "expired", "Released", "lapsed "} {
		if e.Valid() || e.Written() || coord.CheckBrowserControlEnd(e) == nil {
			t.Errorf("%q, which is not on the list, reads as an end", e)
		}
	}
}

// A TAKE IS ANSWERED ONE OF THREE OUTCOMES, and nothing else reads as one.
//
// Mutation: answer every value valid, and the unlisted ones pass.
func TestEveryBrowserControlTakeOutcomeIsAClosedValue(t *testing.T) {
	t.Parallel()
	for _, taken := range []coord.BrowserControlTaken{coord.ControlTaken, coord.ControlKept, coord.ControlHeld} {
		if !taken.Valid() {
			t.Errorf("%q is an outcome and not valid", taken)
		}
	}
	for _, taken := range []coord.BrowserControlTaken{"", "refused", "Taken"} {
		if taken.Valid() {
			t.Errorf("%q, which is no outcome, is valid", taken)
		}
	}
}

// AN OWNER'S KEY READS BACK AS EXACTLY ITS THREE PARTS: the person, the session
// and the take, joined by a separator none of them holds, so two owners never
// share a key and an operator reading the bucket reads one owner.
//
// Mutation: join on ":", which a session's name holds, and it splits wrong.
func TestABrowserControlOwnerKeyReadsBackAsItsThreeParts(t *testing.T) {
	t.Parallel()
	o := controlTake(uuid.New(), "Ada Lovelace").Owner
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	key := o.Key()
	if parts := strings.Split(key, "/"); len(parts) != 3 || parts[0] != o.Person.String() ||
		parts[1] != o.Credential || parts[2] != o.Take.String() {
		t.Fatalf("the key %q splits into %q, want the person, the session and the take", key, parts)
	}
	if len(key) > 118 {
		t.Fatalf("the key %q is %d characters, past the 118 its three parts make", key, len(key))
	}
	for _, other := range []coord.BrowserControlOwner{
		{Person: uuid.New(), Credential: o.Credential, Take: o.Take},
		{Person: o.Person, Credential: iam.SessionName(uuid.NewString()), Take: o.Take},
		{Person: o.Person, Credential: o.Credential, Take: uuid.New()},
	} {
		if other.Key() == key {
			t.Fatalf("two owners share the key %q", key)
		}
	}
}

// A BROWSER CONTROL COUNTS ON ITS CLASS'S ONE COUNTER, for the taker's reason:
// a launch is never leased again once its job ends, and a counter per launch
// would leave a record per launch for the life of the deployment.
//
// Mutation: drop the class from coord.CounterResource, and each launch counts
// on its own.
func TestABrowserControlCountsOnItsClassCounter(t *testing.T) {
	t.Parallel()
	for _, launch := range []uuid.UUID{uuid.New(), uuid.New()} {
		if got := coord.CounterResource(coord.BrowserControlResource(launch)); got != string(coord.ClassBrowserControl) {
			t.Errorf("launch %s's control counts on %q, want its class's %q", launch, got, coord.ClassBrowserControl)
		}
	}
	if seat := coord.SeatResource(uuid.New()); coord.CounterResource(seat) != seat {
		t.Errorf("a seat counts on %q, want its own", coord.CounterResource(seat))
	}
	if got := coord.CounterResource(coord.TakerResource("node-a:1")); got != string(coord.ClassTaker) {
		t.Errorf("a taker lease counts on %q, want its class's %q", got, coord.ClassTaker)
	}
}

// A BROWSER CONTROL'S RESOURCE READS BACK AS ITS LAUNCH, and nothing else does:
// a key written by hand or by another class names no launch, for SeatID's
// reason.
//
// Mutation: accept any uuid.Parse spelling, and the upper-cased one passes.
func TestABrowserControlsResourceReadsBackAsItsLaunch(t *testing.T) {
	t.Parallel()
	launch := uuid.New()
	if got, ok := coord.BrowserControlLaunch(coord.BrowserControlResource(launch)); !ok || got != launch {
		t.Fatalf("the resource of launch %s reads back as (%s, %v)", launch, got, ok)
	}
	for _, resource := range []string{
		coord.SeatResource(launch),
		coord.ClassBrowserControl.Resource("not-a-launch"),
		coord.ClassBrowserControl.Resource(strings.ToUpper(launch.String())),
		coord.BrowserControlResource(uuid.Nil),
	} {
		if got, ok := coord.BrowserControlLaunch(resource); ok {
			t.Errorf("%q reads as launch %s", resource, got)
		}
	}
}

// A BROWSER CONTROL'S RECORD KEEPS THE KEYS IT DOES NOT DECLARE, where they
// were: the top's at the top, and a tenure's with it as it is carried forward.
// Every take, renewal and end rewrites the record, so a build that kept only
// what it declares would strip a newer build's keys on its first write during
// a rolling upgrade. The keys of the record's layout are the package doc's.
//
// Mutation: carry an end rebuilt from its declared fields, and its key goes.
func TestABrowserControlKeepsTheKeysItDoesNotDeclare(t *testing.T) {
	t.Parallel()
	launch := uuid.New()
	at := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	ended := func(epoch int64) coord.BrowserControlJudged {
		return coord.BrowserControlJudged{Epoch: epoch, TakenAt: at, At: at.Add(time.Duration(epoch) * time.Minute)}
	}
	meta, err := coord.BrowserControlTakeMeta(controlTake(launch, "One"), nil, coord.BrowserControlJudged{})
	if err != nil {
		t.Fatal(err)
	}
	if meta, err = coord.BrowserControlTakeMeta(controlTake(launch, "Two"), meta, ended(1)); err != nil {
		t.Fatal(err)
	}
	// A NEWER BUILD'S KEYS, at each level the record has.
	meta["newer_top"] = map[string]any{"kept": []any{"top"}}
	tenureOf(t, meta)["newer_tenure"] = "the second tenure's"
	earlierOf(t, meta)[0]["newer_end"] = "the first tenure's"

	if meta, err = coord.BrowserControlEndMeta(meta, coord.ControlIdle); err != nil {
		t.Fatal(err)
	}
	requireKept(t, "an end's mark", meta, tenureOf(t, meta), earlierOf(t, meta)[0])
	if meta, err = coord.BrowserControlKeptEnd(meta, ended(2)); err != nil {
		t.Fatal(err)
	}
	requireKept(t, "a give-back's mark", meta, tenureOf(t, meta), earlierOf(t, meta)[0])
	if meta, err = coord.BrowserControlTakeMeta(controlTake(launch, "Three"), meta, ended(2)); err != nil {
		t.Fatal(err)
	}
	ends := earlierOf(t, meta)
	if len(ends) != 2 {
		t.Fatalf("the third take carries %d ends, want 2", len(ends))
	}
	requireKept(t, "a take carrying the second tenure forward", meta, ends[0], ends[1])
	if _, kept := tenureOf(t, meta)["newer_tenure"]; kept {
		t.Fatal("the third take's tenure took the second's key, which belongs to the second tenure")
	}
	control, err := coord.DecodeBrowserControl(launch, meta, coord.BrowserControlJudged{
		Epoch: 3, TakenAt: at, Live: true,
	})
	if err != nil || len(control.Earlier) != 2 || control.Earlier[0].EndedBy != coord.ControlIdle {
		t.Fatalf("the record carrying a newer build's keys reads as (%+v, %v)", control, err)
	}
}

// requireKept fails unless the newer build's keys are where they belong: the
// top's in meta, the second tenure's in tenure, the first's in first.
func requireKept(t *testing.T, what string, meta, tenure, first map[string]any) {
	t.Helper()
	for _, kept := range []struct {
		in   map[string]any
		key  string
		want any
	}{
		{meta, "newer_top", map[string]any{"kept": []any{"top"}}},
		{tenure, "newer_tenure", "the second tenure's"},
		{first, "newer_end", "the first tenure's"},
	} {
		if got := kept.in[kept.key]; !reflect.DeepEqual(got, kept.want) {
			t.Fatalf("after %s, %q holds %v, want %v", what, kept.key, got, kept.want)
		}
	}
}

// tenureOf is the record's tenure object, in the layout the package doc gives.
func tenureOf(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()
	tenure, ok := meta["tenure"].(map[string]any)
	if !ok {
		t.Fatalf("the record holds no tenure object: %v", meta)
	}
	return tenure
}

// earlierOf is the ends the record carries, each an object.
func earlierOf(t *testing.T, meta map[string]any) []map[string]any {
	t.Helper()
	raw, _ := meta["earlier"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, end := range raw {
		m, ok := end.(map[string]any)
		if !ok {
			t.Fatalf("the record carries an end that is no object: %v", end)
		}
		out = append(out, m)
	}
	return out
}

// A RECORD THIS CLASS DID NOT WRITE IS UNKNOWN, NEVER AN ANSWER: read as no
// control, it would invite a take of a browser a person may hold, so every
// shape the codec cannot read wraps coord.ErrUnavailable, the third answer.
//
// Mutation: read a tenure with no owner as nobody's, and the first passes.
func TestARecordTheClassDidNotWriteIsUnknown(t *testing.T) {
	t.Parallel()
	launch := uuid.New()
	live := coord.BrowserControlJudged{
		Epoch: 4, TakenAt: time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC), Live: true,
	}
	good, err := coord.BrowserControlTakeMeta(controlTake(launch, "Ada"), nil, coord.BrowserControlJudged{})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		what string
		meta func() map[string]any
	}{
		{"a tenure with no record", func() map[string]any { return nil }},
		{"another launch's record", func() map[string]any {
			m := copyMeta(t, good)
			m["launch"] = uuid.NewString()
			return m
		}},
		{"a tenure naming no person", func() map[string]any {
			m := copyMeta(t, good)
			delete(tenureOf(t, m), "person")
			return m
		}},
		{"an end at an epoch that is no number", func() map[string]any {
			m := copyMeta(t, good)
			m["earlier"] = []any{map[string]any{"epoch": "four"}}
			return m
		}},
		{"ends that are no list", func() map[string]any {
			m := copyMeta(t, good)
			m["earlier"] = "four"
			return m
		}},
	} {
		if control, err := coord.DecodeBrowserControl(launch, bad.meta(), live); !errors.Is(err, coord.ErrUnavailable) {
			t.Errorf("%s reads as (%+v, %v), want an error wrapping coord.ErrUnavailable", bad.what, control, err)
		}
	}
}

// copyMeta is a copy of meta a case may edit, made as a store hands one back.
func copyMeta(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
