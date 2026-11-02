// Package setuptest is the ONE suite every vendor's [setup.Probe] passes, in
// the queuetest / coordtest / integrationtest tradition.
//
// # What it holds a probe to
//
// A probe is the answer to a person pressing Test beside a credential they
// have just pasted, so what it says is acted on at once: a "refused" sends
// them to rotate a key, a "works" lets them save it. Eight vendors deciding
// separately what those words mean would be eight readings of one promise,
// so the promise is written here once and every vendor runs it:
//
//   - every field its requirement list marks [setup.Requirement.Checkable] is
//     answered by a vendor that accepts it as [setup.VerifyWorks], which is
//     the gate holding the list and the probe in step: a checkable field the
//     probe cannot check would offer a Test button that answers nothing;
//   - a vendor refusing it is [setup.VerifyRefused];
//   - a vendor failing on its own side, or not reachable at all, is
//     [setup.VerifyUnreachable] and never a refusal;
//   - a secret field that is NOT checkable is refused as [setup.ErrNotCheckable]
//     rather than answered, since the surface answers those itself from
//     [setup.Requirement.CheckNote];
//   - a field's [setup.Requirement.CheckWith] names other fields of its own
//     list, only on a checkable field, and a credential it names is not
//     checkable on its own, so a form draws one Test control per group;
//   - no detail repeats the credential, and every detail is a sentence.
//
// A suite that agreed only with itself would certify nothing, which is why
// its own test hands it vendors that lie and expects each to go red.
package setuptest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassianhost"
	"github.com/crewlet/crewlet/internal/setup"
)

// TB is the part of *testing.T a case uses, so the suite's own test can drive
// a case against a recorder and assert that a lying vendor fails it.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Vendor is one vendor's probe as the suite drives it.
type Vendor struct {
	// App is the vendor's name as a person reads it.
	App string

	// Requirements is every requirement the vendor's forms offer: both
	// Atlassian deployments where they differ, and a seat's list where the
	// credential lives on a seat.
	Requirements []setup.Requirement

	// Probe builds the vendor's probe reaching srv.
	Probe func(srv *httptest.Server) setup.Probe

	// Values are the other fields a check reads, for a vendor at srv: an
	// instance address, a region, a companion key. The suite answers the
	// credential under check itself.
	Values func(srv *httptest.Server) map[string]string

	// Declared is the Atlassian deployment a check is made under.
	Declared atlassianhost.Deployment

	// Accept, Refuse and Outage answer every request a check makes: the
	// vendor accepting the credential, refusing it the way this vendor
	// refuses, and failing on its own side.
	Accept, Refuse, Outage http.HandlerFunc
}

// Case is one clause of the suite.
type Case struct {
	Name string
	Fn   func(t TB, v Vendor)
}

// Cases is every clause, exported so the suite's own test can hand each to a
// recorder. A vendor calls [Run].
func Cases() []Case {
	return []Case{
		{"every checkable field works against an accepting vendor", checkableFieldsWork},
		{"a refusing vendor is a refusal", refusalsAreRefusals},
		{"an outage is unreachable, never a refusal", outagesAreUnreachable},
		{"a vendor nothing can reach is unreachable", deadVendorIsUnreachable},
		{"a field it cannot check is refused as one", uncheckableFieldsAreRefused},
		{"a check made with other fields names them on a checkable field", checkWithHolds},
	}
}

// Run certifies one vendor's probe.
func Run(t *testing.T, v Vendor) {
	t.Helper()
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Fn(t, v)
		})
	}
}

// secretFor is the credential the suite types into field.
func secretFor(field string) string { return "probe-secret-" + field }

// fields is every requirement of the vendor's lists, once each, split by
// whether it is a checkable credential.
func fields(v Vendor) (checkable, uncheckable []setup.Requirement) {
	seen := map[string]bool{}
	for _, r := range v.Requirements {
		if r.Kind != setup.KindSecret || seen[r.Field] {
			continue
		}
		seen[r.Field] = true
		if r.Checkable {
			checkable = append(checkable, r)
		} else {
			uncheckable = append(uncheckable, r)
		}
	}
	return checkable, uncheckable
}

// check runs one probe call against a vendor answering with h, or, where
// gone is set, against one whose server is closed before the probe is asked.
func check(t TB, v Vendor, field string, h http.HandlerFunc, gone bool) (setup.Verdict, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	probe := v.Probe(srv)
	values := v.Values(srv)
	if gone {
		srv.Close()
	} else {
		defer srv.Close()
	}
	verdict, err := probe(context.Background(), setup.Check{
		Field: field,
		Value: func(name string) string {
			if name == field {
				return secretFor(field)
			}
			return values[name]
		},
		Declared: v.Declared,
	})
	if err == nil {
		sentence(t, v.App, field, verdict)
	}
	return verdict, err
}

// sentence holds every detail to what a person is shown.
func sentence(t TB, app, field string, verdict setup.Verdict) {
	t.Helper()
	detail := verdict.Detail
	switch {
	case strings.TrimSpace(detail) == "":
		t.Errorf("%s.%s answered %s with no sentence", app, field, verdict.Outcome)
	case !strings.HasSuffix(detail, "."):
		t.Errorf("%s.%s: %q is not a sentence", app, field, detail)
	case strings.ContainsRune(detail, '—'):
		t.Errorf("%s.%s: %q carries an em dash", app, field, detail)
	case strings.Contains(detail, secretFor(field)):
		t.Errorf("%s.%s: %q repeats the credential that was checked", app, field, detail)
	}
}

func expect(t TB, v Vendor, h http.HandlerFunc, gone bool, want setup.VerifyOutcome) {
	t.Helper()
	checkable, _ := fields(v)
	if len(checkable) == 0 {
		t.Fatalf("%s marks no field checkable, so this certified nothing", v.App)
	}
	for _, r := range checkable {
		verdict, err := check(t, v, r.Field, h, gone)
		if err != nil {
			t.Errorf("%s.%s: the probe failed rather than answering: %v", v.App, r.Field, err)
			continue
		}
		if verdict.Outcome != want {
			t.Errorf("%s.%s answered %s (%q), want %s",
				v.App, r.Field, verdict.Outcome, verdict.Detail, want)
		}
	}
}

func checkableFieldsWork(t TB, v Vendor) { expect(t, v, v.Accept, false, setup.VerifyWorks) }

func refusalsAreRefusals(t TB, v Vendor) { expect(t, v, v.Refuse, false, setup.VerifyRefused) }

func outagesAreUnreachable(t TB, v Vendor) {
	expect(t, v, v.Outage, false, setup.VerifyUnreachable)
}

func deadVendorIsUnreachable(t TB, v Vendor) {
	expect(t, v, v.Accept, true, setup.VerifyUnreachable)
}

func uncheckableFieldsAreRefused(t TB, v Vendor) {
	_, uncheckable := fields(v)
	for _, r := range uncheckable {
		verdict, err := check(t, v, r.Field, v.Accept, false)
		if !errors.Is(err, setup.ErrNotCheckable) {
			t.Errorf("%s.%s is not checkable and the probe answered %+v, %v: the "+
				"surface answers it from its note, so a probe answering it is a second "+
				"opinion that can disagree", v.App, r.Field, verdict, err)
		}
	}
}

// checkWithHolds holds every [setup.Requirement.CheckWith] to the shape a
// form draws one Test control from: set on a checkable field only, naming
// other fields of the same list once each, and never a credential that has
// a Test control of its own, which would be two controls testing one thing.
func checkWithHolds(t TB, v Vendor) {
	byField := map[string]setup.Requirement{}
	for _, r := range v.Requirements {
		byField[r.Field] = r
	}
	for _, r := range v.Requirements {
		if len(r.CheckWith) == 0 {
			continue
		}
		if !r.Checkable {
			t.Errorf("%s.%s is checked with %v and is not checkable itself",
				v.App, r.Field, r.CheckWith)
		}
		named := map[string]bool{}
		for _, field := range r.CheckWith {
			other, ok := byField[field]
			switch {
			case field == r.Field || named[field]:
				t.Errorf("%s.%s names %q twice in its check", v.App, r.Field, field)
			case !ok:
				t.Errorf("%s.%s is checked with %q, which is not a field of its list",
					v.App, r.Field, field)
			case other.Checkable:
				t.Errorf("%s.%s is checked with %s.%s, which has a Test control of "+
					"its own", v.App, r.Field, v.App, field)
			}
			named[field] = true
		}
	}
}
