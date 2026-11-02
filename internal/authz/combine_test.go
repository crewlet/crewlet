package authz_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A GESTURE OF SEVERAL DECISIONS IS DECIDED AS DECIDE ORDERS ITS OWN CHECKS.
//
// A configuration write is refused part by part, and its answer used to be the
// first part's reason beside a permission written at the call site, whatever
// the parts said: an agent's seat was told it needed admin, a write refused
// for a lead relation and for a credential named the relation alone, and an
// admin whose proof had lapsed was told on every part to ask for admin.
// [authz.Combine] is the one answer: a bar on the caller first, then the
// refusals on authority with the permission that admits every part, then an
// unknown, the stale proof last.
//
// Mutations: ask the proof before authority and the lapsed-proof-beside-a-
// relation row is a step-up; keep the first part's grants rather than the ones
// that admit every part and the tracker-and-knowledge row names tracker; let
// authority beat a bar and the seat row names not_lead; let an unknown beat a
// refusal and the unknown-beside-a-relation row asks the caller to wait; class
// the assistant's two as refusals on authority, or ask its scope before the
// verb it never takes, and the assistant's row names assist_scope.
func TestCombineDecidesAGestureAsDecideOrdersItsChecks(t *testing.T) {
	t.Parallel()
	admin := []iam.Grant{iam.GrantAdmin}
	notLead := authz.Decision{Reason: authz.ReasonNotLead, Grants: admin}
	noGrant := authz.Decision{Reason: authz.ReasonNoGrant, Grants: admin}
	stepUp := authz.Decision{Reason: authz.ReasonStepUp, Recency: iam.RecencyStepUp,
		Grants: admin}
	allowed := authz.Decision{Allowed: true, Reason: authz.ReasonGrant, Grants: admin}
	unknown := authz.Decision{Err: errors.New("the chart is behind")}
	for _, tc := range []struct {
		name string
		ds   []authz.Decision
		want authz.Decision
	}{
		{"every part admitted", []authz.Decision{allowed, allowed}, allowed},
		{"a gesture of no parts", nil,
			authz.Decision{Allowed: true, Reason: authz.ReasonEverybody}},
		{"one decision is itself",
			[]authz.Decision{{Reason: authz.ReasonNoGrant,
				Grants: []iam.Grant{iam.GrantDoclet, iam.GrantWorklet}}},
			authz.Decision{Reason: authz.ReasonNoGrant,
				Grants: []iam.Grant{iam.GrantDoclet, iam.GrantWorklet}}},
		{"a seat beside relations it is refused",
			[]authz.Decision{notLead, {Reason: authz.ReasonSeatRefused}, notLead},
			authz.Decision{Reason: authz.ReasonSeatRefused}},
		{"bars in Decide's order",
			[]authz.Decision{{Reason: authz.ReasonSeatRefused}, {Reason: authz.ReasonStage}},
			authz.Decision{Reason: authz.ReasonStage}},
		{"the assistant's bars beside a relation, the verb it never takes first",
			[]authz.Decision{notLead, {Reason: authz.ReasonAssistScope},
				{Reason: authz.ReasonAssistRefused}},
			authz.Decision{Reason: authz.ReasonAssistRefused}},
		{"the assistant's bars beside a relation, the verb it never takes first",
			[]authz.Decision{notLead, {Reason: authz.ReasonAssistScope},
				{Reason: authz.ReasonAssistRefused}},
			authz.Decision{Reason: authz.ReasonAssistRefused}},
		{"the same relation refused twice", []authz.Decision{notLead, notLead}, notLead},
		{"a relation and a permission", []authz.Decision{notLead, noGrant},
			authz.Decision{Reason: authz.ReasonNoGrant, Grants: admin}},
		{"two apps, which admin alone opens together",
			[]authz.Decision{
				{Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantWorklet}},
				{Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantDoclet}},
			},
			authz.Decision{Reason: authz.ReasonNoGrant, Grants: admin}},
		{"one app any part of which it opens",
			[]authz.Decision{
				{Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantWorklet}},
				{Reason: authz.ReasonNoGrant,
					Grants: []iam.Grant{iam.GrantDoclet, iam.GrantWorklet}},
			},
			authz.Decision{Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantWorklet}}},
		{"a part no permission lifts",
			[]authz.Decision{noGrant, {Reason: authz.ReasonNotSelf}},
			authz.Decision{Reason: authz.ReasonNotSelf}},
		{"a lapsed proof beside a relation", []authz.Decision{stepUp, notLead}, notLead},
		{"an unknown beside a relation", []authz.Decision{unknown, notLead}, notLead},
		{"an unknown beside a lapsed proof", []authz.Decision{stepUp, unknown}, unknown},
		{"only lapsed proofs", []authz.Decision{stepUp, allowed, stepUp}, stepUp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := authz.Combine(tc.ds...)
			if got.Allowed != tc.want.Allowed || got.Reason != tc.want.Reason ||
				got.Recency != tc.want.Recency || !errors.Is(got.Err, tc.want.Err) ||
				!slices.Equal(got.Grants, tc.want.Grants) {
				t.Errorf("Combine = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// EVERY REASON IS EITHER AN ADMISSION OR NOT, AND A REFUSAL ON AUTHORITY OR
// NOT, ONE WAY.
//
// [authz.Reason.Admits] and [authz.Reason.Authority] are what a surface reads
// to tell a part a permission lifts from a bar on the caller, and the sentence
// table in internal/api/httpjson is held to the second. An admission is never
// a refusal on authority. Mutation: class a relation as a bar and its row
// fails.
func TestAReasonIsAnAdmissionOrARefusalOnAuthorityOneWay(t *testing.T) {
	t.Parallel()
	authority := []authz.Reason{authz.ReasonNoGrant, authz.ReasonNotSelf,
		authz.ReasonNotLead, authz.ReasonNotAuthor, authz.ReasonUnnamed,
		authz.ReasonNotShared, authz.ReasonRole}
	admissions := []authz.Reason{authz.ReasonGrant, authz.ReasonSelf,
		authz.ReasonAuthor, authz.ReasonLead, authz.ReasonEverybody, authz.ReasonAsked,
		authz.ReasonShared, authz.ReasonSeatGrant}
	for _, r := range authz.Reasons {
		if got, want := r.Authority(), slices.Contains(authority, r); got != want {
			t.Errorf("%s.Authority() = %v, want %v", r, got, want)
		}
		if got, want := r.Admits(), slices.Contains(admissions, r); got != want {
			t.Errorf("%s.Admits() = %v, want %v", r, got, want)
		}
	}
	if authz.Reason("a_word_nobody_decides_by").Authority() {
		t.Error("a reason this build does not know is no refusal on authority")
	}
}
