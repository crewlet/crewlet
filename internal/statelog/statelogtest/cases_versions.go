package statelogtest

import (
	"fmt"
	"testing"
)

// Stamping is every contract a domain can break in the VERSION it stamps on
// what it writes, checked by encoding records and reading their envelopes
// back.
//
// # Why the stamp is certified here rather than in each domain
//
// The version a record carries is what an OLDER build decides by: at or below
// what it reads, it decodes the record and applies what it understands; above,
// it retains the record whole and applies it after its upgrade. So the stamp
// is a promise to a build that does not exist yet in this tree — the previous
// one — and the only place that promise can be checked is from the record's
// own bytes. Two failures, both silent until a rolling upgrade:
//
//   - A record carrying a new field stamped too LOW is applied by an older node
//     with the field dropped, and that node's rows for the object differ from
//     its peers' for good.
//   - A record carrying nothing new stamped too HIGH is retained by every older
//     node, and so is everything its scope meets, for the whole upgrade.
//
// EXPORTED AND RETURNING ERRORS for [Declaration]'s reason: the suite's own
// tests hand it a domain that stamps wrongly and read the verdict back.
func Stamping(c Candidate) []error {
	var out []error
	out = append(out, stampsBaseRecords(c)...)
	return append(out, stampsCarriedFields(c)...)
}

// stampsBaseRecords: a record carrying no versioned field is readable by every
// build there is.
func stampsBaseRecords(c Candidate) []error {
	name := c.Domain.Name()
	var out []error
	for _, kind := range c.Kinds {
		body, err := c.Encode(kind, "suite-object", "suite-stamp-"+kind, 0)
		if err != nil {
			out = append(out, fmt.Errorf("%s could not encode a %s record with "+
				"its version left to the encoder: %w", name, kind, err))
			continue
		}
		env, err := c.Domain.Envelope(body)
		if err != nil {
			out = append(out, fmt.Errorf("%s: envelope a %s record: %w", name, kind, err))
			continue
		}
		if env.V != 1 || !env.ReadableBy(1) {
			out = append(out, fmt.Errorf("%s stamped a %s record carrying no "+
				"versioned field at version %d, want 1 — every build older than "+
				"this one would retain it, and everything its scope meets, for "+
				"the length of a rolling upgrade", name, kind, env.V))
		}
	}
	return out
}

// stampsCarriedFields: a record carrying a versioned field is stamped at that
// field's version, exactly — held back by every build that predates the field
// and applied by this one.
func stampsCarriedFields(c Candidate) []error {
	name := c.Domain.Name()
	var out []error
	if len(c.Fields) != 0 && c.Carrying == nil {
		return append(out, fmt.Errorf("%s declares versioned fields and no "+
			"record carrying one", name))
	}
	reads := c.Domain.RecordVersion()
	for _, field := range c.Fields {
		body, err := c.Carrying(field)
		if err != nil {
			out = append(out, fmt.Errorf("%s could not encode a record carrying "+
				"%s: %w", name, field.Name, err))
			continue
		}
		env, err := c.Domain.Envelope(body)
		if err != nil {
			out = append(out, fmt.Errorf("%s: envelope a record carrying %s: %w",
				name, field.Name, err))
			continue
		}
		switch {
		case env.ReadableBy(field.Since - 1):
			out = append(out, fmt.Errorf("%s stamped a record carrying %s at "+
				"version %d, and a build reading %d — the last one without the "+
				"field — would decode it, drop the field and apply the rest; "+
				"either the encoder ignores the field table or the table's path "+
				"%v is not where the encoder writes the field",
				name, field.Name, env.V, field.Since-1, field.Path))
		case env.V != field.Since:
			out = append(out, fmt.Errorf("%s stamped a record carrying only %s "+
				"at version %d, want %d — above the field's own version, every "+
				"build that could read it retains it anyway",
				name, field.Name, env.V, field.Since))
		case !env.ReadableBy(reads):
			out = append(out, fmt.Errorf("%s stamped a record carrying %s at "+
				"version %d and reads %d — the build that wrote it would retain "+
				"its own write", name, field.Name, env.V, reads))
		}
	}
	return out
}

// runVersions reports what [Stamping] found, one invariant per case.
func runVersions(t *testing.T, new Factory) {
	t.Helper()
	t.Run("a record without a new field still encodes at version one", func(t *testing.T) {
		c := new(t)
		requireKinds(t, c)
		for _, err := range stampsBaseRecords(c) {
			t.Error(err)
		}
	})
	t.Run("a record carrying a new field is retained, not applied, by a build that cannot read it", func(t *testing.T) {
		for _, err := range stampsCarriedFields(new(t)) {
			t.Error(err)
		}
	})
}
