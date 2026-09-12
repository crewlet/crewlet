package config

import (
	"fmt"
	"strings"
)

// Warning is a configuration that is VALID and probably not what somebody
// meant — or valid and carrying a consequence they should know about before
// they find it out.
//
// # Why warnings are their own channel and not errors
//
// A refusal says "this cannot run". Every one of these can, and some of them
// describe the correct configuration for somebody's deployment: a development
// topology, a company that has decided not to back up, a rename that will
// re-onboard a team on purpose. Turning any of them into an error would make
// the engine refuse a decision that was not its to make.
//
// # And why they are not just log lines
//
// A log line is read after the thing has already happened. These are read by
// `crewlet validate`, which is what a person runs BEFORE applying a config and
// what a CI step runs before a deploy — the one moment the consequence can
// still change the decision.
type Warning struct {
	// Path is the field it is about, in the same dotted form a validation
	// error uses, so one authoring loop can jump to either.
	Path string

	// Message says what will happen and what to do instead, in that order.
	// A warning that only says something is unusual is one people learn to
	// ignore.
	Message string
}

// String renders a warning as one line.
func (w Warning) String() string {
	if w.Path == "" {
		return w.Message
	}
	return w.Path + ": " + w.Message
}

// Warnings is everything valid about this bootstrap that its author should
// still know.
//
// SEPARATE FROM Validate, and it takes no error path: a document that does not
// validate has problems worth fixing first, and a warning printed beside a
// refusal is noise at exactly the moment somebody is reading carefully.
func (b *Bootstrap) Warnings() []Warning {
	var out []Warning

	// A DECLINED FSYNC IS A DECISION WITH A NUMBER ON IT. It is legitimate
	// — a replicated fleet across power domains genuinely trades a window
	// for throughput — and the window is what an operator should have said
	// out loud rather than inherited.
	if !b.Stream.SyncAlways() {
		out = append(out, Warning{
			Path: "stream.sync",
			Message: fmt.Sprintf(
				"an acknowledged write may be up to %s behind the disk. That is a "+
					"trade against a correlated power loss taking every replica at "+
					"once — write `always` if this deployment cannot afford it",
				b.Stream.SyncInterval()),
		})
	}

	// A FLEET THAT TRIMS ONLY WHAT IT HAS BEEN TOLD IS OFF-SITE does not
	// trim until somebody tells it, and the log grows until its ceiling
	// refuses writes. That is the configuration working as asked; it is
	// also a state nobody discovers until the refusal.
	if b.Stream.TrackerRetention.Floor() == BackupFloorOperator {
		out = append(out, Warning{
			Path: "stream.tracker_retention.backup_floor",
			Message: "`operator` means the trim advances only as far as somebody " +
				"has acknowledged a backup. Until the first acknowledgement the log " +
				"is never trimmed, and it grows until `stream.tracker_log_max_bytes` " +
				"starts refusing writes",
		})
	}

	// NOBODY OWNS THE BACKUP. A company that never backs up never trims —
	// the log is the only copy of what no node has applied yet — so "who
	// is responsible for this" has a real answer on every deployment that
	// intends to keep working, and nowhere to write it is how it goes
	// unasked.
	if strings.TrimSpace(b.Retention.BackupOwner) == "" {
		out = append(out, Warning{
			Path: "retention.backup_owner",
			Message: "nobody is named as this deployment's backup owner. The trim " +
				"stops when the newest backup ages past " +
				"`stream.tracker_retention.backup_max_age`, and the alarm that says " +
				"so has nobody to name",
		})
	}

	// AN EMBEDDED STREAM WITH NOWHERE TO PERSIST loses everything on a
	// restart. It is the right configuration for a test and for an
	// ingress-only node, and the wrong one for anything holding a company.
	if b.Stream.Type != StreamNATS && strings.TrimSpace(b.Stream.StoreDir) == "" {
		out = append(out, Warning{
			Path: "stream.store_dir",
			Message: "an embedded stream with no store directory keeps everything in " +
				"memory: a restart loses every mailbox, every coordination record and " +
				"the company's own history. Correct for a test; not for a node that " +
				"holds seats",
		})
	}
	return out
}

// Warnings is everything valid about this company that its author should
// still know.
func (c *Company) Warnings() []Warning {
	var out []Warning

	// A UNIT WITH NO ID IS KEYED ON ITS NAME, and a name is prose: it gets
	// renamed for the reasons prose does. Two different things follow, and
	// the warning names both because fixing one does not fix the other.
	for u := range c.organization().AllUnits() {
		if strings.TrimSpace(u.ID) != "" {
			continue
		}
		out = append(out, Warning{
			Path: "units." + u.Name,
			Message: "this unit has no `id`, so everything durable is keyed on its " +
				"NAME — renaming it moves what is filed under it. Giving it an id " +
				"fixes that, and does NOT stop a rename re-onboarding the seats " +
				"beneath it: onboarding turns on the name, because the name is what " +
				"an agent reads as its team",
		})
	}
	return out
}

// CheckTiers holds the rules that need BOTH documents, and it exists because
// neither tier can see the other.
//
// Tier A is the operator's and Tier B is the founder's; each validates alone,
// and a rule about the pair has nowhere else to live. There is exactly one
// today, and it is worth the seam: it turns a permanent, unrecoverable state
// into a refusal at the moment somebody could still choose otherwise.
func CheckTiers(boot *Bootstrap, company *Company) error {
	var p problems
	if boot == nil || company == nil {
		return nil
	}

	// A NATIVE TRACKER ON AN IN-MEMORY STREAM IS UNRECOVERABLE, and that is
	// why it is an error rather than the warning Tier A raises alone.
	//
	// The engine's own tracker keeps its write-ahead log on the stream. An
	// embedded server with no store directory keeps its streams in MEMORY,
	// so a restart recreates them empty — and a node whose durable tables
	// are ahead of a stream that has restarted from nothing cannot tell
	// "the log was trimmed" from "the log is a different log", refuses to
	// serve, and stays refused: every snapshot it could adopt is above the
	// recreated stream too.
	//
	// It is an error rather than a warning because there is no correct
	// deployment it describes. A company on a vendor tracker starts no log
	// at all and is unaffected, which is why the rule needs both documents.
	if company.TrackerBackendFor() == TrackerNative &&
		boot.Stream.Type != StreamNATS &&
		strings.TrimSpace(boot.Stream.StoreDir) == "" {
		p.add("stream.store_dir", ErrMissing,
			"this company runs the engine's own tracker, whose log lives on the "+
				"stream — and an embedded stream with no store directory keeps its "+
				"streams in memory, so a restart recreates them empty and this node "+
				"refuses to serve the tracker permanently. Name a directory")
	}
	return p.err()
}
