package iam

import (
	"slices"
	"testing"
)

// A ZERO POSTURE CONFERS NOTHING, WHATEVER THE ROW HOLDS.
//
// The zero is what a node whose configuration reached the request path without
// `api.auth.admin` would compose every principal under, and reading it as
// either value would be guessing which one the operator meant — so it fails
// closed, as an empty ceiling always did: a non-nil empty answer for admin, an
// app, and an unknown spelling alike. The control is a valid posture over the
// same row, which carries something. Mutation: treat an invalid posture as
// serve and every case here goes red.
func TestAZeroPostureConfersNothing(t *testing.T) {
	t.Parallel()
	row := []Grant{GrantAdmin, GrantWorklet}
	for _, posture := range []AdminPosture{"", "Serve", "open"} {
		got := posture.Effective(row)
		if got == nil || len(got) != 0 {
			t.Errorf("%q confers %#v, want a non-nil empty set", posture, got)
		}
		if posture.Valid() {
			t.Errorf("%q validated, and only %v are postures", posture, AdminPostures)
		}
	}
	if len(AdminServe.Effective(row)) == 0 {
		t.Fatal("the control conferred nothing either, so the cases above prove nothing")
	}
}

// A REFUSING NODE KEEPS EVERY APP AND CLAMPS ADMIN AWAY.
//
// [Expand] runs before the clamp, so an administrator signed in on a node that
// refuses admin keeps the tracker and the knowledge base — the clamp takes
// away running the company and nothing a colleague has. And the answer is the
// KNOWN set in [AllGrants] order: an unknown name stays in the row and grants
// nothing here. Mutation: clamp before expanding and the administrator on the
// refusing node is left with nothing.
func TestARefusingNodeKeepsEveryApp(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		posture AdminPosture
		held    []Grant
		want    []Grant
	}{
		{"an administrator on a serving node", AdminServe,
			[]Grant{GrantAdmin}, AllGrants},
		{"an administrator on a refusing node", AdminRefuse,
			[]Grant{GrantAdmin}, Apps},
		{"one app on a refusing node", AdminRefuse,
			[]Grant{GrantDoclet}, []Grant{GrantDoclet}},
		{"an unknown name is dropped and the rest ordered", AdminServe,
			[]Grant{GrantDoclet, "swarm", GrantWorklet},
			[]Grant{GrantWorklet, GrantDoclet}},
		{"nothing held", AdminServe, nil, []Grant{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.posture.Effective(c.held); !slices.Equal(got, c.want) {
				t.Errorf("%s.Effective(%v) = %v, want %v", c.posture, c.held, got, c.want)
			}
		})
	}
}
