package gitlet_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A tips query names 1 to MaxTips whole ids once each and any revision as base.
// Mutations: take MaxTips+1; leave GitTips out of Unstampable; take an abbreviation.
func TestATipsQueryValidates(t *testing.T) {
	t.Parallel()
	asker := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
	repo := "01920000-0000-7000-8000-000000000001"
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%040x", i+1)
		}
		return out
	}
	q := func(oids []string, base string) gitlet.GitQuery {
		return gitlet.GitQuery{Kind: gitlet.GitTips, RepoID: repo, Asker: asker,
			Tips: &gitlet.TipsQuery{OIDs: oids, Base: base}}
	}
	for _, tc := range []struct {
		name, field string
		q           gitlet.GitQuery
	}{
		{"no oids", "tips.oids", q(nil, "")},
		{"past MaxTips", "tips.oids", q(ids(gitlet.MaxTips+1), "")},
		{"an abbreviation", "tips.oids[1]", q([]string{ids(1)[0], ids(2)[1][:12]}, "")},
		{"the zero id", "tips.oids[0]", q([]string{gitlet.ZeroOID}, "")},
		{"a repeat", "tips.oids[2]", q(append(ids(2), ids(1)[0]), "")},
		{"a base that is a ref", "tips.base", q(ids(1), "main")},
		{"another kind's query", "compare", func() gitlet.GitQuery {
			r := q(ids(1), "")
			r.Compare = &gitlet.CompareQuery{Base: ids(1)[0], Head: ids(1)[0]}
			return r
		}()},
	} {
		err := tc.q.Validate()
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%s: Validate = %v, want ErrInvalid naming %s", tc.name, err, tc.field)
		}
	}
	ok := q(ids(gitlet.MaxTips), ids(1)[0][:7])
	ok.Unstamped = true
	if err := ok.Validate(); err != nil {
		t.Errorf("a hundred whole ids against an abbreviated base, unstamped: %v, want a read", err)
	}
}
