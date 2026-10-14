package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// A NODE THAT SERVES NO CODE HOST SAYS WHY, FROM WHAT THE FLEET ACTIVATED.
// Read off this node's published epoch alone, a node building its first
// company's code host, or one whose apply of it failed, told a person who had
// just turned code.native on that the company hosts no code — the epoch an
// apply has not yet published, or never will, is the previous one. So the
// answer reads the activation this node's reconciler last read: not hosted
// only where neither this node's epoch nor that activation sets code.native;
// applying while it is behind and has not failed; the apply failed when it
// did on a revision it read as hosting code; unknown when it failed before it
// could read the revision at all; and a restart owed where the epoch it
// serves hosts code its runtime was started without.
//
// WHAT THE NODE NEVER READ IT NEVER CLAIMS: an apply that failed before the
// decode — a fetch on a coordination blip, a revision this node's key cannot
// open — says it cannot tell whether the company hosts code, never the failed
// apply's "which hosts code", which told a company that never set code.native
// that it had.
//
// Mutation: answer from the published epoch alone, and every behind case
// reads not hosted; answer every failed apply as one that hosts code, and the
// unread one goes red.
func TestANodeServingNoCodeHostSaysWhy(t *testing.T) {
	t.Parallel()
	hosting := parseCompany(t, codeHostAlone)
	plain := parseCompany(t, nativeCleanupCompany)
	now := time.Now()
	for _, tc := range []struct {
		name     string
		serves   *config.Company
		progress *applyProgress
		want     error
	}{
		{"no company and no reconciler", nil, nil, ErrCodeNotHosted},
		{"a company that hosts no code, current", plain, &applyProgress{applied: 3, target: 3}, ErrCodeNotHosted},
		{"nothing activated yet", nil, &applyProgress{}, ErrCodeNotHosted},
		{"an epoch that hosts code on a runtime started without it", hosting,
			&applyProgress{applied: 3, target: 3}, ErrCodeNeedsRestart},
		{"behind, the apply not yet begun", plain, &applyProgress{applied: 3, target: 4}, ErrCodeApplying},
		{"behind, applying a revision that hosts code", plain,
			&applyProgress{applied: 3, target: 4, attemptStarted: now, targetRead: true, targetCode: true},
			ErrCodeApplying},
		{"a first company's code host being built", nil,
			&applyProgress{target: 1, attemptStarted: now, targetRead: true, targetCode: true},
			ErrCodeApplying},
		{"behind, retrying a revision that hosts code", plain,
			&applyProgress{applied: 3, target: 4, attempts: 1, attemptStarted: now, targetRead: true,
				targetCode: true}, ErrCodeApplying},
		{"the apply of a revision that hosts code failed", plain,
			&applyProgress{applied: 3, target: 4, attempts: 1, targetRead: true, targetCode: true},
			ErrCodeApplyFailed},
		{"a first company's code host that could not be built", nil,
			&applyProgress{target: 1, attempts: 1, targetRead: true, targetCode: true}, ErrCodeApplyFailed},
		{"the apply of a revision it could not read failed", plain,
			&applyProgress{applied: 3, target: 4, attempts: 2}, ErrCodeUnknown},
		{"a first company's revision it could not read", nil,
			&applyProgress{target: 1, attempts: 1}, ErrCodeUnknown},
		{"retrying a revision it could not read", plain,
			&applyProgress{applied: 3, target: 4, attempts: 1, attemptStarted: now}, ErrCodeApplying},
		{"the apply of a revision that hosts no code failed", plain,
			&applyProgress{applied: 3, target: 4, attempts: 1, targetRead: true}, ErrCodeNotHosted},
		{"applying a revision that hosts no code", plain,
			&applyProgress{applied: 3, target: 4, attemptStarted: now, targetRead: true}, ErrCodeNotHosted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{}
			if tc.serves != nil {
				e.epoch.current.Store(&Company{Config: tc.serves})
			}
			if tc.progress != nil {
				r := &Reconciler{}
				r.publish(*tc.progress)
				e.reconciler.Store(r)
			}
			got := e.CodeUnserved()
			if !errors.Is(got, tc.want) {
				t.Fatalf("CodeUnserved = %v, want %v", got, tc.want)
			}
			if (errors.Is(tc.want, ErrCodeApplying) || errors.Is(tc.want, ErrCodeApplyFailed) ||
				errors.Is(tc.want, ErrCodeUnknown)) &&
				!strings.Contains(got.Error(), fmt.Sprintf("(epoch %d)", tc.progress.target)) {
				t.Errorf("CodeUnserved = %v, which names no epoch", got)
			}
			// Whatever the sentinels are called, an answer about a revision
			// this node has not read carries no sentence that says the
			// company hosts code.
			if tc.progress != nil && !tc.progress.targetRead &&
				(errors.Is(got, ErrCodeApplyFailed) || strings.Contains(got.Error(), ErrCodeApplyFailed.Error())) {
				t.Errorf("CodeUnserved = %v about a revision this node never read, which says the "+
					"company hosts code", got)
			}
		})
	}
}

// THE RECONCILER NOTES WHAT THE FLEET ACTIVATED SAYS OF THE CODE HOST, for the
// target it is applying alone: a revision read for an epoch the progress has
// moved past says nothing of the one it is on.
func TestTheReconcilerNotesWhatItsTargetSaysOfTheCodeHost(t *testing.T) {
	t.Parallel()
	r := &Reconciler{}
	r.publish(applyProgress{applied: 3, target: 4})
	r.noteTargetCode(3, true)
	if p := r.snapshot(); p.targetRead {
		t.Fatalf("a revision of an epoch past was noted: %+v", p)
	}
	r.noteTargetCode(4, true)
	if p := r.snapshot(); !p.targetRead || !p.targetCode {
		t.Fatalf("the target's revision was not noted: %+v", p)
	}
}
