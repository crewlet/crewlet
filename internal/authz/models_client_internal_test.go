package authz

import "testing"

// THE MODELS SCREEN OFFERS A WRITE ONLY TO AN ADMIN, because every route it
// writes through is decided by the admin grant alone. The dashboard knows a
// reader's grants, never the engine's actions, so Settings › Models & Fleets gates each
// write control on `admin` (dashboard/src/routes/settings/models/data.ts).
// That holds only while each of these actions is [ClassAdmin]: one moved to
// another class would leave a control missing for a reader the engine admits,
// or offer one it refuses. This is where such a move is noticed.
//
// `PUT /config` is decided as [ActionOrgWrite] per place it reaches, and a
// setting it changes (`providers.llm`, `assistant`) as [ActionConfigWrite]. A
// seats-only change may be admitted to a lead; the screen offers admin only,
// withholding a write the engine would admit, never offering one it refuses.
func TestTheModelsScreensWritesAreAnAdminsAlone(t *testing.T) {
	t.Parallel()
	for _, a := range []Action{
		ActionSecretWrite, // a credential's test and store, a device sign-in and its cancel
		ActionModelsCheck, // a model's test, and a host login's check
		ActionModelsRetry, // a sit-out's retry and clear
		ActionConfigWrite, // removing a model, and the wizard's change to the document
	} {
		r, ok := rules[a]
		if !ok {
			t.Errorf("%s has no rule, and the Models screen writes through it", a)
			continue
		}
		if r.class != ClassAdmin {
			t.Errorf("%s is decided by class %q, not %q: the Models screen gates it on the admin grant, so change both together", a, r.class, ClassAdmin)
		}
	}
}
