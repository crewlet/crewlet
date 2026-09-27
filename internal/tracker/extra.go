package tracker

import "github.com/crewlet/crewlet/internal/jsoncarry"

// Every object the tracker stores or puts on its log carries the members this
// build does not know, at every depth, through
// [github.com/crewlet/crewlet/internal/jsoncarry] — whose package doc is the
// contract.
//
// # Why every object, and at every depth
//
// A task commit decodes the stored document, merges the record onto it and
// encodes the result ([Applier.applyTask]); a writer decides a change by
// reading an object, changing it and publishing the whole of it; and the
// change feed renders a record by encoding it again ([recordBody]). A member a
// newer build added — to a task, or to a checklist, a field definition or the
// notification inside one — that this build decoded into a struct with no
// field for it would be missing from what this node writes back, while every
// peer that knows it keeps it: two copies of one log holding different rows
// for the same record, which is the one property the replicated estate may not
// lose. So every type below holds what it does not know in its
// Extra and writes it back, and a type added to a stored object or a record
// does the same — TestEveryObjectTheTrackerSharesCarries walks every one.
//
// # What does not carry, and why
//
// [Subject] is an address: every member of it is recovered from the log
// subject a record is published on ([ParseSubject]), so there is nothing a
// carry could hold that the address does not already say. [ScopeSet] is
// written by a method of its own, as a bare sentinel string or a list of
// terms. [ScopeTerm], [Relation], [ChecklistItem] and [Delta] are compared by
// value where the tree reads them — checklistAssignees compares two items with
// `!=`, and tests compare terms, relations and deltas the same way — which a
// type holding a map does not compile under. And [GoalTarget] is embedded in
// [GoalTargetRow], which a MarshalJSON of its own would be promoted onto: the
// row would be written as the bare target, without the progress beside it.

// MarshalJSON writes [Task.Extra] back beside the members this build knows.
func (t Task) MarshalJSON() ([]byte, error) {
	type fields Task
	return jsoncarry.Marshal(fields(t), t.Extra)
}

// UnmarshalJSON keeps every member of the stored task this build does not know
// in [Task.Extra].
func (t *Task) UnmarshalJSON(b []byte) error {
	type fields Task
	return jsoncarry.Unmarshal(b, (*fields)(t), &t.Extra)
}

// MarshalJSON writes [TaskPatch.Extra] back beside the members this build
// knows.
func (p TaskPatch) MarshalJSON() ([]byte, error) {
	type fields TaskPatch
	return jsoncarry.Marshal(fields(p), p.Extra)
}

// UnmarshalJSON keeps every member of the patch this build does not know in
// [TaskPatch.Extra].
func (p *TaskPatch) UnmarshalJSON(b []byte) error {
	type fields TaskPatch
	return jsoncarry.Unmarshal(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes [Counter.Extra] back beside the members this build knows.
func (c Counter) MarshalJSON() ([]byte, error) {
	type fields Counter
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the counter this build does not know in
// [Counter.Extra].
func (c *Counter) UnmarshalJSON(b []byte) error {
	type fields Counter
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [RankOrder.Extra] back beside the members this build
// knows.
func (o RankOrder) MarshalJSON() ([]byte, error) {
	type fields RankOrder
	return jsoncarry.Marshal(fields(o), o.Extra)
}

// UnmarshalJSON keeps every member of the order this build does not know in
// [RankOrder.Extra].
func (o *RankOrder) UnmarshalJSON(b []byte) error {
	type fields RankOrder
	return jsoncarry.Unmarshal(b, (*fields)(o), &o.Extra)
}

// MarshalJSON writes [Placement.Extra] back beside the members this build
// knows.
func (p Placement) MarshalJSON() ([]byte, error) {
	type fields Placement
	return jsoncarry.Marshal(fields(p), p.Extra)
}

// UnmarshalJSON keeps every member of the placement this build does not know in
// [Placement.Extra].
func (p *Placement) UnmarshalJSON(b []byte) error {
	type fields Placement
	return jsoncarry.Unmarshal(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes [Eviction.Extra] back beside the members this build knows.
func (e Eviction) MarshalJSON() ([]byte, error) {
	type fields Eviction
	return jsoncarry.Marshal(fields(e), e.Extra)
}

// UnmarshalJSON keeps every member of the eviction this build does not know in
// [Eviction.Extra].
func (e *Eviction) UnmarshalJSON(b []byte) error {
	type fields Eviction
	return jsoncarry.Unmarshal(b, (*fields)(e), &e.Extra)
}

// MarshalJSON writes [Generation.Extra] back beside the members this build
// knows.
func (g Generation) MarshalJSON() ([]byte, error) {
	type fields Generation
	return jsoncarry.Marshal(fields(g), g.Extra)
}

// UnmarshalJSON keeps every member of the generation this build does not know
// in [Generation.Extra].
func (g *Generation) UnmarshalJSON(b []byte) error {
	type fields Generation
	return jsoncarry.Unmarshal(b, (*fields)(g), &g.Extra)
}

// MarshalJSON writes [TypeCatalogue.Extra] back beside the members this build
// knows.
func (c TypeCatalogue) MarshalJSON() ([]byte, error) {
	type fields TypeCatalogue
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the catalogue this build does not know in
// [TypeCatalogue.Extra].
func (c *TypeCatalogue) UnmarshalJSON(b []byte) error {
	type fields TypeCatalogue
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [TaskType.Extra] back beside the members this build knows.
func (t TaskType) MarshalJSON() ([]byte, error) {
	type fields TaskType
	return jsoncarry.Marshal(fields(t), t.Extra)
}

// UnmarshalJSON keeps every member of the type this build does not know in
// [TaskType.Extra].
func (t *TaskType) UnmarshalJSON(b []byte) error {
	type fields TaskType
	return jsoncarry.Unmarshal(b, (*fields)(t), &t.Extra)
}

// MarshalJSON writes [FieldCatalogue.Extra] back beside the members this build
// knows.
func (c FieldCatalogue) MarshalJSON() ([]byte, error) {
	type fields FieldCatalogue
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the catalogue this build does not know in
// [FieldCatalogue.Extra].
func (c *FieldCatalogue) UnmarshalJSON(b []byte) error {
	type fields FieldCatalogue
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [FieldDef.Extra] back beside the members this build knows.
func (f FieldDef) MarshalJSON() ([]byte, error) {
	type fields FieldDef
	return jsoncarry.Marshal(fields(f), f.Extra)
}

// UnmarshalJSON keeps every member of the field this build does not know in
// [FieldDef.Extra].
func (f *FieldDef) UnmarshalJSON(b []byte) error {
	type fields FieldDef
	return jsoncarry.Unmarshal(b, (*fields)(f), &f.Extra)
}

// MarshalJSON writes [FieldConfig.Extra] back beside the members this build
// knows.
func (c FieldConfig) MarshalJSON() ([]byte, error) {
	type fields FieldConfig
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the configuration this build does not
// know in [FieldConfig.Extra].
func (c *FieldConfig) UnmarshalJSON(b []byte) error {
	type fields FieldConfig
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [Option.Extra] back beside the members this build knows.
func (o Option) MarshalJSON() ([]byte, error) {
	type fields Option
	return jsoncarry.Marshal(fields(o), o.Extra)
}

// UnmarshalJSON keeps every member of the option this build does not know in
// [Option.Extra].
func (o *Option) UnmarshalJSON(b []byte) error {
	type fields Option
	return jsoncarry.Unmarshal(b, (*fields)(o), &o.Extra)
}

// MarshalJSON writes [Rollup.Extra] back beside the members this build knows.
func (r Rollup) MarshalJSON() ([]byte, error) {
	type fields Rollup
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member of the rollup this build does not know in
// [Rollup.Extra].
func (r *Rollup) UnmarshalJSON(b []byte) error {
	type fields Rollup
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [Project.Extra] back beside the members this build knows.
func (p Project) MarshalJSON() ([]byte, error) {
	type fields Project
	return jsoncarry.Marshal(fields(p), p.Extra)
}

// UnmarshalJSON keeps every member of the project this build does not know in
// [Project.Extra].
func (p *Project) UnmarshalJSON(b []byte) error {
	type fields Project
	return jsoncarry.Unmarshal(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes [TagSet.Extra] back beside the members this build knows.
func (s TagSet) MarshalJSON() ([]byte, error) {
	type fields TagSet
	return jsoncarry.Marshal(fields(s), s.Extra)
}

// UnmarshalJSON keeps every member of the tag set this build does not know in
// [TagSet.Extra].
func (s *TagSet) UnmarshalJSON(b []byte) error {
	type fields TagSet
	return jsoncarry.Unmarshal(b, (*fields)(s), &s.Extra)
}

// MarshalJSON writes [Tag.Extra] back beside the members this build knows.
func (t Tag) MarshalJSON() ([]byte, error) {
	type fields Tag
	return jsoncarry.Marshal(fields(t), t.Extra)
}

// UnmarshalJSON keeps every member of the tag this build does not know in
// [Tag.Extra].
func (t *Tag) UnmarshalJSON(b []byte) error {
	type fields Tag
	return jsoncarry.Unmarshal(b, (*fields)(t), &t.Extra)
}

// MarshalJSON writes [View.Extra] back beside the members this build knows.
func (v View) MarshalJSON() ([]byte, error) {
	type fields View
	return jsoncarry.Marshal(fields(v), v.Extra)
}

// UnmarshalJSON keeps every member of the view this build does not know in
// [View.Extra].
func (v *View) UnmarshalJSON(b []byte) error {
	type fields View
	return jsoncarry.Unmarshal(b, (*fields)(v), &v.Extra)
}

// MarshalJSON writes [Container.Extra] back beside the members this build
// knows.
func (c Container) MarshalJSON() ([]byte, error) {
	type fields Container
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the container this build does not know in
// [Container.Extra].
func (c *Container) UnmarshalJSON(b []byte) error {
	type fields Container
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [Goal.Extra] back beside the members this build knows.
func (g Goal) MarshalJSON() ([]byte, error) {
	type fields Goal
	return jsoncarry.Marshal(fields(g), g.Extra)
}

// UnmarshalJSON keeps every member of the goal this build does not know in
// [Goal.Extra].
func (g *Goal) UnmarshalJSON(b []byte) error {
	type fields Goal
	return jsoncarry.Unmarshal(b, (*fields)(g), &g.Extra)
}

// MarshalJSON writes [GoalUpdate.Extra] back beside the members this build
// knows.
func (u GoalUpdate) MarshalJSON() ([]byte, error) {
	type fields GoalUpdate
	return jsoncarry.Marshal(fields(u), u.Extra)
}

// UnmarshalJSON keeps every member of the update this build does not know in
// [GoalUpdate.Extra].
func (u *GoalUpdate) UnmarshalJSON(b []byte) error {
	type fields GoalUpdate
	return jsoncarry.Unmarshal(b, (*fields)(u), &u.Extra)
}

// MarshalJSON writes [Person.Extra] back beside the members this build knows.
func (p Person) MarshalJSON() ([]byte, error) {
	type fields Person
	return jsoncarry.Marshal(fields(p), p.Extra)
}

// UnmarshalJSON keeps every member of the person this build does not know in
// [Person.Extra].
func (p *Person) UnmarshalJSON(b []byte) error {
	type fields Person
	return jsoncarry.Unmarshal(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes [InboxEntry.Extra] back beside the members this build
// knows.
func (e InboxEntry) MarshalJSON() ([]byte, error) {
	type fields InboxEntry
	return jsoncarry.Marshal(fields(e), e.Extra)
}

// UnmarshalJSON keeps every member of the entry this build does not know in
// [InboxEntry.Extra].
func (e *InboxEntry) UnmarshalJSON(b []byte) error {
	type fields InboxEntry
	return jsoncarry.Unmarshal(b, (*fields)(e), &e.Extra)
}

// MarshalJSON writes [Favorite.Extra] back beside the members this build knows.
func (f Favorite) MarshalJSON() ([]byte, error) {
	type fields Favorite
	return jsoncarry.Marshal(fields(f), f.Extra)
}

// UnmarshalJSON keeps every member of the favourite this build does not know in
// [Favorite.Extra].
func (f *Favorite) UnmarshalJSON(b []byte) error {
	type fields Favorite
	return jsoncarry.Unmarshal(b, (*fields)(f), &f.Extra)
}

// MarshalJSON writes [Position.Extra] back beside the members this build knows.
func (p Position) MarshalJSON() ([]byte, error) {
	type fields Position
	return jsoncarry.Marshal(fields(p), p.Extra)
}

// UnmarshalJSON keeps every member of the position this build does not know in
// [Position.Extra].
func (p *Position) UnmarshalJSON(b []byte) error {
	type fields Position
	return jsoncarry.Unmarshal(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes [Spend.Extra] back beside the members this build knows.
func (s Spend) MarshalJSON() ([]byte, error) {
	type fields Spend
	return jsoncarry.Marshal(fields(s), s.Extra)
}

// UnmarshalJSON keeps every member of the spend this build does not know in
// [Spend.Extra].
func (s *Spend) UnmarshalJSON(b []byte) error {
	type fields Spend
	return jsoncarry.Unmarshal(b, (*fields)(s), &s.Extra)
}

// MarshalJSON writes [Tombstone.Extra] back beside the members this build
// knows.
func (t Tombstone) MarshalJSON() ([]byte, error) {
	type fields Tombstone
	return jsoncarry.Marshal(fields(t), t.Extra)
}

// UnmarshalJSON keeps every member of the tombstone this build does not know in
// [Tombstone.Extra].
func (t *Tombstone) UnmarshalJSON(b []byte) error {
	type fields Tombstone
	return jsoncarry.Unmarshal(b, (*fields)(t), &t.Extra)
}

// MarshalJSON writes [Checklist.Extra] back beside the members this build
// knows.
func (c Checklist) MarshalJSON() ([]byte, error) {
	type fields Checklist
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the checklist this build does not know in
// [Checklist.Extra].
func (c *Checklist) UnmarshalJSON(b []byte) error {
	type fields Checklist
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [Comment.Extra] back beside the members this build knows.
func (c Comment) MarshalJSON() ([]byte, error) {
	type fields Comment
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// UnmarshalJSON keeps every member of the comment this build does not know in
// [Comment.Extra].
func (c *Comment) UnmarshalJSON(b []byte) error {
	type fields Comment
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes [BodyRevision.Extra] back beside the members this build
// knows.
func (r BodyRevision) MarshalJSON() ([]byte, error) {
	type fields BodyRevision
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member of the revision this build does not know in
// [BodyRevision.Extra].
func (r *BodyRevision) UnmarshalJSON(b []byte) error {
	type fields BodyRevision
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [KeyAlias.Extra] back beside the members this build knows.
func (a KeyAlias) MarshalJSON() ([]byte, error) {
	type fields KeyAlias
	return jsoncarry.Marshal(fields(a), a.Extra)
}

// UnmarshalJSON keeps every member of the alias this build does not know in
// [KeyAlias.Extra].
func (a *KeyAlias) UnmarshalJSON(b []byte) error {
	type fields KeyAlias
	return jsoncarry.Unmarshal(b, (*fields)(a), &a.Extra)
}

// MarshalJSON writes [KeyMint.Extra] back beside the members this build knows.
func (m KeyMint) MarshalJSON() ([]byte, error) {
	type fields KeyMint
	return jsoncarry.Marshal(fields(m), m.Extra)
}

// UnmarshalJSON keeps every member of the mint this build does not know in
// [KeyMint.Extra].
func (m *KeyMint) UnmarshalJSON(b []byte) error {
	type fields KeyMint
	return jsoncarry.Unmarshal(b, (*fields)(m), &m.Extra)
}

// MarshalJSON writes [MutationRecord.Extra] back beside the members this build
// knows.
func (r MutationRecord) MarshalJSON() ([]byte, error) {
	type fields MutationRecord
	return jsoncarry.Marshal(fields(r), r.Extra)
}

// UnmarshalJSON keeps every member of the record this build does not know in
// [MutationRecord.Extra].
func (r *MutationRecord) UnmarshalJSON(b []byte) error {
	type fields MutationRecord
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// MarshalJSON writes [Notify.Extra] back beside the members this build knows.
func (n Notify) MarshalJSON() ([]byte, error) {
	type fields Notify
	return jsoncarry.Marshal(fields(n), n.Extra)
}

// UnmarshalJSON keeps every member of the notification this build does not know
// in [Notify.Extra].
func (n *Notify) UnmarshalJSON(b []byte) error {
	type fields Notify
	return jsoncarry.Unmarshal(b, (*fields)(n), &n.Extra)
}

// MarshalJSON writes [Snapshot.Extra] back beside the members this build knows.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	type fields Snapshot
	return jsoncarry.Marshal(fields(s), s.Extra)
}

// UnmarshalJSON keeps every member of the snapshot this build does not know in
// [Snapshot.Extra].
func (s *Snapshot) UnmarshalJSON(b []byte) error {
	type fields Snapshot
	return jsoncarry.Unmarshal(b, (*fields)(s), &s.Extra)
}

// MarshalJSON writes [TaskParty.Extra] back beside the members this build
// knows.
func (p TaskParty) MarshalJSON() ([]byte, error) {
	type fields TaskParty
	return jsoncarry.Marshal(fields(p), p.Extra)
}

// UnmarshalJSON keeps every member of the party this build does not know in
// [TaskParty.Extra].
func (p *TaskParty) UnmarshalJSON(b []byte) error {
	type fields TaskParty
	return jsoncarry.Unmarshal(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes [TurnSpend.Extra] back beside the members this build
// knows.
func (s TurnSpend) MarshalJSON() ([]byte, error) {
	type fields TurnSpend
	return jsoncarry.Marshal(fields(s), s.Extra)
}

// UnmarshalJSON keeps every member of the spend this build does not know in
// [TurnSpend.Extra].
func (s *TurnSpend) UnmarshalJSON(b []byte) error {
	type fields TurnSpend
	return jsoncarry.Unmarshal(b, (*fields)(s), &s.Extra)
}
