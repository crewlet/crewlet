package builtin

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN AUTHORIZER THAT CONTRADICTS ITSELF IS A FAULT, NOT A REFUSAL.
//
// A non-answer comment from somebody admitted only as the person an ask names
// is refused by asking the table again on the bare kind. An authorizer that
// ADMITS there has answered one question about one principal two ways in one
// call, which the real table cannot, so the call is this node's fault: classed
// internal_error, never a refusal (ErrRefused) naming no reason or permission.
//
// Mutation: answer the contradiction with authorityRefusal(…, ErrRefused) again
// and this goes red.
func TestAnAuthorizerThatContradictsItselfIsAFault(t *testing.T) {
	t.Parallel()
	admits := WorkDeps{Authorize: func(context.Context, authz.Action, authz.Object) error {
		return nil
	}}
	got := admits.answerOnly(everyGrant(), "this comment answers no open question")
	if !got.Failed || tools.RefusalOf(*got) != tools.RefusalInternalError ||
		errors.Is(got.Cause, ErrRefused) {
		t.Errorf("an authorizer admitting the bare kind answered %+v (class %q), "+
			"want a fault classed internal_error that is no refusal", got,
			tools.RefusalOf(*got))
	}

	// THE CONTROL: the table's own refusal of the bare kind is still the
	// refusal, carrying what the caller may do instead.
	refuses := WorkDeps{Authorize: func(context.Context, authz.Action, authz.Object) error {
		return &DecisionRefused{Action: authz.ActionWorkComment, Decision: authz.Decision{
			Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantWorklet}}}
	}}
	got = refuses.answerOnly(everyGrant(), "this comment answers no open question")
	var refused *DecisionRefused
	if !got.Failed || !errors.As(got.Cause, &refused) {
		t.Errorf("the table's refusal of the bare kind answered %+v, want that refusal", got)
	}
}
