package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// BrowserControlTTL is how long a person's control of a run's browser lasts
// from each take or renewal ([BrowserControls]): 30 seconds. Its caller renews
// every third of it, so one lost renewal is survived, the next landing with a
// third to spare, and a closed tab frees the browser within half a minute. Two
// lost in a row are not: the third renewal reaches the store at the deadline
// at best, and a renewal at or past it is refused as lapsed. The verbs take a
// TTL so the contract suite can lapse a short one; every caller passes this.
const BrowserControlTTL = 30 * time.Second

// MaxBrowserControlEnds is how many ended tenures a browser control's record
// carries beside its newest: four, as many ends as a run's box keeps untold
// and its model is told of at once, so an end older than four later tenures
// is one nothing downstream still tells. At its worst the record then fits
// [MaxLeaseMetaBytes] (TestABrowserControlAtItsWorstFitsTheLeaseMetaBound).
const MaxBrowserControlEnds = 4

// MaxBrowserControlHolderName is the most characters a holder's display name
// may have: 200, a round figure just under the most the record can carry. It
// holds five holders' names, its newest tenure's and its carried ends', and
// with each character at its worst, a six-byte escape, five of 200 still leave
// the record within [MaxLeaseMetaBytes]
// (TestABrowserControlAtItsWorstFitsTheLeaseMetaBound). A person's display
// name may run longer (internal/iamdomain's MaxName is 256 bytes) or hold what
// a holder may not, so a caller conforms it first ([BrowserControlHolderName]).
const MaxBrowserControlHolderName = 200

// BrowserControlResource names the lease of one executor launch's browser
// control ([ClassBrowserControl]).
func BrowserControlResource(launch uuid.UUID) string {
	return ClassBrowserControl.Resource(launch.String())
}

// BrowserControlLaunch recovers the launch from a browser control's resource.
// It reports false for another class's resource and for a name that is not a
// canonical uuid, for [SeatID]'s reason.
func BrowserControlLaunch(resource string) (uuid.UUID, bool) {
	name, ok := ClassBrowserControl.Name(resource)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(name)
	if err != nil || id == uuid.Nil || id.String() != name {
		return uuid.Nil, false
	}
	return id, true
}

// BrowserControlEnd is why a tenure of a browser control ended: a closed list,
// so an unknown value read off a newer build's record is a value, not a panic.
type BrowserControlEnd string

// The ends of a browser control. Every one but [ControlLapsed] is written by
// the node that ends the tenure; a lapse is no write.
const (
	// ControlReleased is the person letting go.
	ControlReleased BrowserControlEnd = "released"
	// ControlHandedBack is the person handing the browser back to the run.
	ControlHandedBack BrowserControlEnd = "handed_back"
	// ControlLapsed is no renewal within the TTL, by the store's clock.
	ControlLapsed BrowserControlEnd = "lapsed"
	// ControlIdle is the person holding it without acting for too long.
	ControlIdle BrowserControlEnd = "idle"
	// ControlJobEnded is the run's job ending under it.
	ControlJobEnded BrowserControlEnd = "job_ended"
	// ControlRevoked is the person refused the browser at a renewal or an act.
	ControlRevoked BrowserControlEnd = "revoked"
)

// BrowserControlEnds are the six.
var BrowserControlEnds = []BrowserControlEnd{
	ControlReleased, ControlHandedBack, ControlLapsed, ControlIdle, ControlJobEnded, ControlRevoked,
}

// Valid reports whether an end is one this build knows.
func (e BrowserControlEnd) Valid() bool { return slices.Contains(BrowserControlEnds, e) }

// Written reports whether a writer may name this end: every one but a lapse,
// which the store reads from its own clock and nobody writes.
func (e BrowserControlEnd) Written() bool { return e.Valid() && e != ControlLapsed }

// CheckBrowserControlEnd refuses an end a writer may not name, before anything
// is sent: a lapse, or a value this build does not know.
func CheckBrowserControlEnd(by BrowserControlEnd) error {
	if by.Written() {
		return nil
	}
	var written []BrowserControlEnd
	for _, e := range BrowserControlEnds {
		if e.Written() {
			written = append(written, e)
		}
	}
	return fmt.Errorf("coord: a browser control cannot be ended as %q: a writer names one of %s, and a "+
		"lapse is the store's to read from its own clock", by, words.Or(written))
}

// BrowserControlOwner is who holds a browser control: a person, through the
// browser session they act in, under one take, a uuid their panel mints per
// take gesture, so two tabs of one session are two owners.
type BrowserControlOwner struct {
	Person     uuid.UUID
	Credential string
	Take       uuid.UUID
}

// browserControlOwnerSeparator joins an owner's three parts in its key: none
// of them can hold it (a uuid, `session:` and a uuid, a uuid).
const browserControlOwnerSeparator = "/"

// Key is the owner as its lease names it, the parts joined by a separator none
// of them holds, so a key reads back as one owner: at most 118 characters.
func (o BrowserControlOwner) Key() string {
	return o.Person.String() + browserControlOwnerSeparator + o.Credential +
		browserControlOwnerSeparator + o.Take.String()
}

// Validate refuses an owner that breaks the class's rules, naming the field.
func (o BrowserControlOwner) Validate() error {
	switch {
	case o.Person == uuid.Nil:
		return errors.New("coord: a browser control's Owner.Person is the nil uuid: an owner is a person")
	case !iam.ValidSessionName(o.Credential):
		return fmt.Errorf("coord: a browser control's Owner.Credential %q names no browser session: control "+
			"is a person's own, in a dashboard session, so a credential is `session:<lineage>`", o.Credential)
	case o.Take == uuid.Nil:
		return errors.New("coord: a browser control's Owner.Take is the nil uuid: each take gesture mints one")
	}
	return nil
}

// BrowserControlHolder is who a browser control's owner is to everybody
// watching: their human seat and their display name, made a holder's name by
// [BrowserControlHolderName].
type BrowserControlHolder struct {
	Seat uuid.UUID
	Name string
}

// validate refuses a holder that breaks the record's bound: a seat, and a name
// of 1 to [MaxBrowserControlHolderName] characters of valid UTF-8 with no
// control character, each of which encoding/json writes in at most six bytes.
func (h BrowserControlHolder) validate() error {
	switch n := utf8.RuneCountInString(h.Name); {
	case h.Seat == uuid.Nil:
		return errors.New("coord: a browser control's Holder.Seat is the nil uuid: a holder is a human seat")
	case !utf8.ValidString(h.Name):
		return errors.New("coord: a browser control's Holder.Name is not valid UTF-8")
	case n == 0 || n > MaxBrowserControlHolderName:
		return fmt.Errorf("coord: a browser control's Holder.Name has %d characters, outside 1 to %d",
			n, MaxBrowserControlHolderName)
	case strings.IndexFunc(h.Name, unicode.IsControl) >= 0:
		return errors.New("coord: a browser control's Holder.Name holds a control character")
	}
	return nil
}

// BrowserControlHolderName is the name a holder is shown by, made from display,
// a person's display name as the identity estate keeps it, which may run past
// the bound or hold what a holder may not: each run of bytes that is not UTF-8
// becomes U+FFFD and each control character a space, cut to
// [MaxBrowserControlHolderName] characters. A name the rule passes comes back
// as it is unless it is only white space: a name that shows nothing is made
// from fallback alike, the person's human seat's handle, as the dashboard names
// a seat with no name. It is empty only where fallback shows nothing either,
// which a take refuses as a caller's mistake.
func BrowserControlHolderName(display, fallback string) string {
	if name := holderNameOf(display); name != "" {
		return name
	}
	return holderNameOf(fallback)
}

// holderNameOf is name made to keep a holder's rule, or empty where it shows
// nothing.
func holderNameOf(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(name, string(utf8.RuneError)))
	if utf8.RuneCountInString(name) > MaxBrowserControlHolderName {
		name = string([]rune(name)[:MaxBrowserControlHolderName])
	}
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return name
}

// BrowserControlTake is one take of a launch's browser: the launch, its turn
// and its seat, who takes it, and who they are to everybody watching.
type BrowserControlTake struct {
	Launch uuid.UUID
	Turn   uuid.UUID
	Seat   uuid.UUID
	Owner  BrowserControlOwner
	Holder BrowserControlHolder
}

// Validate refuses a take that breaks the record's rules, naming the field: a
// caller's bug, so an error and never a refusal.
func (t BrowserControlTake) Validate() error {
	switch {
	case t.Launch == uuid.Nil:
		return errors.New("coord: a browser control take's Launch is the nil uuid")
	case t.Turn == uuid.Nil:
		return errors.New("coord: a browser control take's Turn is the nil uuid")
	case t.Seat == uuid.Nil:
		return errors.New("coord: a browser control take's Seat is the nil uuid")
	}
	if err := t.Owner.Validate(); err != nil {
		return err
	}
	return t.Holder.validate()
}

// CheckBrowserControlLaunch refuses a launch a caller got wrong: the nil uuid,
// which names no launch.
func CheckBrowserControlLaunch(launch uuid.UUID) error {
	if launch == uuid.Nil {
		return errors.New("coord: a browser control's launch is the nil uuid")
	}
	return nil
}

// CheckBrowserControlTenure refuses a renewal's or an end's arguments a caller
// got wrong, before anything is sent: a launch, a valid owner, a real epoch.
func CheckBrowserControlTenure(launch uuid.UUID, owner BrowserControlOwner, epoch int64) error {
	if err := CheckBrowserControlLaunch(launch); err != nil {
		return err
	}
	if epoch < 1 {
		return fmt.Errorf("coord: a browser control's epoch %d names no tenure: a fencing token starts at 1", epoch)
	}
	return owner.Validate()
}

// BrowserControlTenure is one tenure of a browser control. Both instants are on
// the store's clock and for a person to read: nothing decides by them, as
// nothing decides by [Lease.AcquiredAt]. EndedBy is empty while it is live.
type BrowserControlTenure struct {
	Epoch   int64
	Owner   BrowserControlOwner
	Holder  BrowserControlHolder
	TakenAt time.Time
	EndedBy BrowserControlEnd
	// EndedAt is the instant of the write that ended it, or the deadline that
	// passed for a lapse.
	EndedAt time.Time
}

// Ended reports whether the tenure has ended.
func (t BrowserControlTenure) Ended() bool { return t.EndedBy != "" }

// BrowserControl is one launch's browser control as the store read it, judged
// on the store's clock.
type BrowserControl struct {
	Launch uuid.UUID
	Turn   uuid.UUID
	Seat   uuid.UUID

	// Current is the newest tenure the record holds, live or ended, and nil
	// where it holds none (a take that is minting, or was given back).
	Current *BrowserControlTenure

	// Earlier is the ends carried forward, newest first, at most
	// [MaxBrowserControlEnds].
	Earlier []BrowserControlTenure
}

// Held is the tenure holding the browser: Current while it has not ended. It
// reads no clock: the store judged the record as it read it, so this is the
// store's word, as [Holder]'s is.
func (c BrowserControl) Held() (BrowserControlTenure, bool) {
	if c.Current == nil || c.Current.Ended() {
		return BrowserControlTenure{}, false
	}
	return *c.Current, true
}

// Of is how the tenure at epoch stands, live or ended, and false for an epoch
// the record no longer carries.
func (c BrowserControl) Of(epoch int64) (BrowserControlTenure, bool) {
	if c.Current != nil && c.Current.Epoch == epoch {
		return *c.Current, true
	}
	for _, t := range c.Earlier {
		if t.Epoch == epoch {
			return t, true
		}
	}
	return BrowserControlTenure{}, false
}

// BrowserControlTaken is how a take was answered: a closed outcome.
type BrowserControlTaken string

const (
	// ControlTaken is a new tenure, at an epoch above every token the class's
	// counter minted before.
	ControlTaken BrowserControlTaken = "taken"
	// ControlKept is the owner's own live tenure, its epoch kept, so a retried
	// take is one take.
	ControlKept BrowserControlTaken = "kept"
	// ControlHeld is another owner's live tenure, which the answer's Current
	// names.
	ControlHeld BrowserControlTaken = "held"
)

// Valid reports whether an outcome is one this build knows.
func (t BrowserControlTaken) Valid() bool {
	return t == ControlTaken || t == ControlKept || t == ControlHeld
}

// BrowserControlWrite is one write to a browser control a watch hears: its
// launch, and the epoch of the tenure the write left or ended (zero for a
// record the store removed, as [LeaseWrite.Epoch] says).
type BrowserControlWrite struct {
	Launch uuid.UUID
	Epoch  int64
}

// BrowserControls is a person's control of the browser an executor launch runs
// (the package doc, "A person's control of a run's browser"), certified by one
// suite (coordtest.RunBrowserControls). Every error is UNKNOWN.
type BrowserControls interface {
	// TakeBrowserControl takes t's launch for its owner, holding it for ttl:
	// taken, kept, or held, its Current naming the other owner's live tenure.
	TakeBrowserControl(ctx context.Context, t BrowserControlTake, ttl time.Duration) (BrowserControl,
		BrowserControlTaken, error)

	// RenewBrowserControl extends the owner's live tenure at epoch by ttl on the
	// store's clock, answering the record as written; false, with the record
	// read, where that tenure is not live for that owner. A lapse is never renewed.
	RenewBrowserControl(ctx context.Context, launch uuid.UUID, owner BrowserControlOwner, epoch int64,
		ttl time.Duration) (BrowserControl, bool, error)

	// EndBrowserControl ends the owner's live tenure at epoch as by, in one
	// write, answering the record as written; false, with the record read,
	// where that tenure is not live for that owner. A lapse is never written.
	EndBrowserControl(ctx context.Context, launch uuid.UUID, owner BrowserControlOwner, epoch int64,
		by BrowserControlEnd) (BrowserControl, bool, error)

	// BrowserControl reads a launch's record, false where it has none.
	BrowserControl(ctx context.Context, launch uuid.UUID) (BrowserControl, bool, error)

	// WatchBrowserControls streams the launch of every write to the class made
	// after it returns, until ctx ends or the watch fails and the channel
	// closes: a hint, never an answer, as [Backend.WatchLive] is.
	WatchBrowserControls(ctx context.Context) (<-chan BrowserControlWrite, error)
}

// BrowserControlWrites maps a watch of the class's leases ([Backend.WatchLive])
// to the launches written: every store's WatchBrowserControls. It closes what
// it returns once writes closes or ctx ends, which ends its goroutine.
func BrowserControlWrites(ctx context.Context, writes <-chan LeaseWrite) <-chan BrowserControlWrite {
	out := make(chan BrowserControlWrite)
	go func() {
		defer close(out)
		for w := range writes {
			launch, ok := BrowserControlLaunch(w.Resource)
			if !ok {
				continue
			}
			select {
			case out <- BrowserControlWrite{Launch: launch, Epoch: w.Epoch}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// BrowserControlGranted is a take's answer where its store granted the lease:
// the control its record holds, live, taken where the grant began a new tenure
// and kept where it kept the owner's own.
func BrowserControlGranted(launch uuid.UUID, lease Lease, fresh bool) (BrowserControl, BrowserControlTaken,
	error) {

	control, err := DecodeBrowserControl(launch, lease.Meta, BrowserControlJudged{
		Epoch: lease.Epoch, TakenAt: lease.AcquiredAt, Live: true,
	})
	switch {
	case err != nil:
		return BrowserControl{}, "", err
	case fresh:
		return control, ControlTaken, nil
	}
	return control, ControlKept, nil
}

// --- the record --------------------------------------------------------------

// BrowserControlJudged is how the store holding a browser control's record
// judged the tenure it holds, on its own clock: what the codec cannot read from
// the record's Meta. Only a store builds one (coord/kv, coord/memory).
type BrowserControlJudged struct {
	// Epoch is the record's fencing token, 0 where it holds no tenure.
	Epoch int64
	// TakenAt is the tenure's start ([Lease.AcquiredAt]).
	TakenAt time.Time
	// Live reports a tenure the store holds live.
	Live bool
	// Lapsed reports one whose deadline passed with its owner still set.
	Lapsed bool
	// At is a lapse's deadline, or else the instant of the write that left the
	// record, which is an end's where the Meta names none.
	At time.Time
}

// The keys of a browser control's record, in its lease's Meta: short, since the
// record crosses on every renewal ([MaxLeaseMetaBytes]). Every value is a JSON
// string, an epoch included, which a JSON number would round past 2^53.
const (
	keyLaunch     = "launch"
	keyTurn       = "turn"
	keySeat       = "seat"
	keyTenure     = "tenure"
	keyEarlier    = "earlier"
	keyEpoch      = "epoch"
	keyTakenAt    = "taken_at"
	keyPerson     = "person"
	keyCredential = "credential"
	keyTake       = "take"
	keyHolderSeat = "holder_seat"
	keyHolderName = "holder_name"
	keyEndedBy    = "ended_by"
	keyEndedAt    = "ended_at"
)

// BrowserControlTakeMeta is the Meta a take writes over prior, the Meta of the
// record it replaces, whose tenure the store judged as was: the take's own
// tenure, and the tenure prior held carried forward as an end before the ends
// prior carries, the newest [MaxBrowserControlEnds]. Every key this build does
// not declare is kept where it was (the package doc).
func BrowserControlTakeMeta(t BrowserControlTake, prior map[string]any, was BrowserControlJudged) (map[string]any, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	meta, err := copyRecord(prior)
	if err != nil {
		return nil, err
	}
	ends, err := carriedEnds(meta, was)
	if err != nil {
		return nil, err
	}
	meta[keyLaunch], meta[keyTurn], meta[keySeat] = t.Launch.String(), t.Turn.String(), t.Seat.String()
	meta[keyTenure] = map[string]any{
		keyPerson: t.Owner.Person.String(), keyCredential: t.Owner.Credential, keyTake: t.Owner.Take.String(),
		keyHolderSeat: t.Holder.Seat.String(), keyHolderName: t.Holder.Name,
	}
	delete(meta, keyEarlier)
	if len(ends) > 0 {
		meta[keyEarlier] = ends
	}
	return meta, nil
}

// carriedEnds is the ends a take over meta carries: the tenure meta holds as
// judged, where it ended, before the ends meta carries, the newest
// [MaxBrowserControlEnds]. A record holding no tenure adds none.
func carriedEnds(meta map[string]any, was BrowserControlJudged) ([]any, error) {
	earlier, err := endsIn(meta)
	if err != nil {
		return nil, err
	}
	if was.Epoch == 0 {
		return earlier, nil
	}
	if was.Live {
		return nil, fmt.Errorf("coord: a take replaces no live tenure, and the record holds epoch %d live",
			was.Epoch)
	}
	tenure, err := tenureIn(meta)
	if err != nil {
		return nil, err
	}
	by, at, err := endOf(tenure, was)
	if err != nil {
		return nil, err
	}
	tenure[keyEpoch] = strconv.FormatInt(was.Epoch, 10)
	tenure[keyTakenAt] = stamp(was.TakenAt)
	tenure[keyEndedBy], tenure[keyEndedAt] = string(by), stamp(at)
	ends := append([]any{tenure}, earlier...)
	return ends[:min(len(ends), MaxBrowserControlEnds)], nil
}

// BrowserControlEndMeta is meta with its tenure marked ended as by, for the
// tombstone the end's write leaves: the instant is that write's own stamp.
func BrowserControlEndMeta(meta map[string]any, by BrowserControlEnd) (map[string]any, error) {
	if err := CheckBrowserControlEnd(by); err != nil {
		return nil, err
	}
	out, err := copyRecord(meta)
	if err != nil {
		return nil, err
	}
	tenure, err := tenureIn(out)
	if err != nil {
		return nil, err
	}
	tenure[keyEndedBy] = string(by)
	delete(tenure, keyEndedAt)
	return out, nil
}

// BrowserControlKeptEnd is meta with how its tenure ended, as judged, written
// into it, for a write that clears the record's owner and moves its stamp
// without ending anything: a failed claim's give-back over it. A record
// holding no tenure, or a live one, is left as it is.
func BrowserControlKeptEnd(meta map[string]any, was BrowserControlJudged) (map[string]any, error) {
	if was.Epoch == 0 || was.Live || len(meta) == 0 {
		return meta, nil
	}
	out, err := copyRecord(meta)
	if err != nil {
		return nil, err
	}
	tenure, err := tenureIn(out)
	if err != nil {
		return nil, err
	}
	by, at, err := endOf(tenure, was)
	if err != nil {
		return nil, err
	}
	tenure[keyEndedBy], tenure[keyEndedAt] = string(by), stamp(at)
	return out, nil
}

// DecodeBrowserControl is the browser control launch's record holds: meta, its
// lease's Meta, with its tenure as the store judged it. An error is a record
// this class did not write, which no reader may take for an answer.
func DecodeBrowserControl(launch uuid.UUID, meta map[string]any, was BrowserControlJudged) (BrowserControl, error) {
	c := BrowserControl{Launch: launch}
	if len(meta) == 0 {
		if was.Epoch == 0 {
			return c, nil
		}
		return BrowserControl{}, recordErr("carries nothing at epoch %d", was.Epoch)
	}
	written, err := uuidIn(meta, keyLaunch)
	switch {
	case err != nil:
		return BrowserControl{}, err
	case written != launch:
		return BrowserControl{}, recordErr("names launch %s under %s's key", written, launch)
	}
	if c.Turn, err = uuidIn(meta, keyTurn); err != nil {
		return BrowserControl{}, err
	}
	if c.Seat, err = uuidIn(meta, keySeat); err != nil {
		return BrowserControl{}, err
	}
	if was.Epoch > 0 {
		var current BrowserControlTenure
		if current, err = currentOf(meta, was); err != nil {
			return BrowserControl{}, err
		}
		c.Current = &current
	}
	ends, err := endsIn(meta)
	if err != nil {
		return BrowserControl{}, err
	}
	for _, end := range ends {
		var t BrowserControlTenure
		if t, err = endedTenure(end.(map[string]any)); err != nil {
			return BrowserControl{}, err
		}
		c.Earlier = append(c.Earlier, t)
	}
	return c, nil
}

// currentOf is the tenure meta holds, at was's epoch and start, ended as judged.
func currentOf(meta map[string]any, was BrowserControlJudged) (BrowserControlTenure, error) {
	tenure, err := tenureIn(meta)
	if err != nil {
		return BrowserControlTenure{}, err
	}
	t := BrowserControlTenure{Epoch: was.Epoch, TakenAt: was.TakenAt.UTC()}
	if t.Owner, t.Holder, err = whoIn(tenure); err != nil {
		return BrowserControlTenure{}, err
	}
	if !was.Live {
		if t.EndedBy, t.EndedAt, err = endOf(tenure, was); err != nil {
			return BrowserControlTenure{}, err
		}
	}
	return t, nil
}

// endOf is how tenure ended, as the store judged it: a lapse at its deadline
// where its owner is still set; otherwise the cause its record names (a
// generic Release names none, which reads as released) at the instant it names,
// or at the write that left the record.
func endOf(tenure map[string]any, was BrowserControlJudged) (BrowserControlEnd, time.Time, error) {
	if was.Lapsed {
		return ControlLapsed, was.At.UTC(), nil
	}
	by, err := stringIn(tenure, keyEndedBy, true)
	if err != nil {
		return "", time.Time{}, err
	}
	at, err := instantIn(tenure, keyEndedAt, true)
	if err != nil {
		return "", time.Time{}, err
	}
	if by == "" {
		by = string(ControlReleased)
	}
	if at.IsZero() {
		at = was.At.UTC()
	}
	return BrowserControlEnd(by), at, nil
}

// endedTenure decodes one end a record carries.
func endedTenure(end map[string]any) (BrowserControlTenure, error) {
	raw, err := stringIn(end, keyEpoch, false)
	if err != nil {
		return BrowserControlTenure{}, err
	}
	t := BrowserControlTenure{}
	if t.Epoch, err = strconv.ParseInt(raw, 10, 64); err != nil || t.Epoch < 1 {
		return BrowserControlTenure{}, recordErr("carries an end at epoch %q", raw)
	}
	if t.Owner, t.Holder, err = whoIn(end); err != nil {
		return BrowserControlTenure{}, err
	}
	if t.TakenAt, err = instantIn(end, keyTakenAt, false); err != nil {
		return BrowserControlTenure{}, err
	}
	by, err := stringIn(end, keyEndedBy, false)
	if err != nil {
		return BrowserControlTenure{}, err
	}
	t.EndedBy = BrowserControlEnd(by)
	if t.EndedAt, err = instantIn(end, keyEndedAt, false); err != nil {
		return BrowserControlTenure{}, err
	}
	return t, nil
}

// whoIn is the owner and holder a tenure's map names.
func whoIn(tenure map[string]any) (BrowserControlOwner, BrowserControlHolder, error) {
	var (
		o   BrowserControlOwner
		h   BrowserControlHolder
		err error
	)
	if o.Person, err = uuidIn(tenure, keyPerson); err != nil {
		return o, h, err
	}
	if o.Credential, err = stringIn(tenure, keyCredential, false); err != nil {
		return o, h, err
	}
	if o.Take, err = uuidIn(tenure, keyTake); err != nil {
		return o, h, err
	}
	if h.Seat, err = uuidIn(tenure, keyHolderSeat); err != nil {
		return o, h, err
	}
	h.Name, err = stringIn(tenure, keyHolderName, false)
	return o, h, err
}

// tenureIn is the map of the tenure meta holds, which a caller may edit: meta
// is the caller's own copy ([copyRecord]).
func tenureIn(meta map[string]any) (map[string]any, error) {
	tenure, ok := meta[keyTenure].(map[string]any)
	if !ok {
		return nil, recordErr("holds no tenure")
	}
	return tenure, nil
}

// endsIn is the ends meta carries, each checked to be a map, and nil for none.
func endsIn(meta map[string]any) ([]any, error) {
	raw, ok := meta[keyEarlier]
	if !ok {
		return nil, nil
	}
	ends, ok := raw.([]any)
	if !ok {
		return nil, recordErr("carries its ends as %T", raw)
	}
	for _, end := range ends {
		if _, ok := end.(map[string]any); !ok {
			return nil, recordErr("carries an end as %T", end)
		}
	}
	return ends, nil
}

// stringIn is m's string at key; absent is "" where optional, else an error.
func stringIn(m map[string]any, key string, optional bool) (string, error) {
	raw, ok := m[key]
	switch {
	case !ok && optional:
		return "", nil
	case !ok:
		return "", recordErr("names no %q", key)
	}
	s, isString := raw.(string)
	if !isString {
		return "", recordErr("holds %q as %T", key, raw)
	}
	return s, nil
}

// uuidIn is m's uuid at key, which every record names.
func uuidIn(m map[string]any, key string) (uuid.UUID, error) {
	s, err := stringIn(m, key, false)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, recordErr("holds %q as %q, which is no uuid", key, s)
	}
	return id, nil
}

// instantIn is m's instant at key, in UTC; absent is the zero instant where
// optional, else an error.
func instantIn(m map[string]any, key string, optional bool) (time.Time, error) {
	s, err := stringIn(m, key, optional)
	if err != nil || s == "" {
		return time.Time{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, recordErr("holds %q as %q, which is no instant", key, s)
	}
	return at.UTC(), nil
}

// copyRecord is a copy of meta the caller may edit, made through the encoding
// a store writes it in, so nothing of the store's own map is shared and every
// value is the shape a store hands back.
func copyRecord(meta map[string]any) (map[string]any, error) {
	if len(meta) == 0 {
		return map[string]any{}, nil
	}
	raw, err := EncodeLeaseRecord(meta)
	if err != nil {
		return nil, fmt.Errorf("coord: a browser control's record does not encode: %w", err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("coord: a browser control's record does not decode: %w", err)
	}
	return out, nil
}

// recordErr is a record this class did not write: UNKNOWN to every reader,
// since answering "not held" from it would invite a take of a live control.
func recordErr(format string, args ...any) error {
	return fmt.Errorf("%w: a browser control's record "+format, append([]any{ErrUnavailable}, args...)...)
}
