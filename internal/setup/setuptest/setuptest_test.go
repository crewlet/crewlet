package setuptest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// honest is a vendor whose probe keeps every clause: one GET to /me, its
// status judged by the shared rule.
func honest() setuptest.Vendor {
	return vendor(func(srv *httptest.Server) setup.Probe {
		return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
			if check.Field != "token" {
				return setup.Verdict{}, setup.NotCheckable("Acme", check.Field)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/me", nil)
			if err != nil {
				return setup.Verdict{}, err
			}
			res, err := srv.Client().Do(req)
			if err != nil {
				return setup.Judge(setup.Failure{App: "Acme", Err: err}, check.Secret()), nil
			}
			defer res.Body.Close()
			_, _ = io.Copy(io.Discard, res.Body)
			if res.StatusCode >= 300 {
				return setup.Judge(setup.Failure{App: "Acme", Err: errors.New("acme: refused"),
					Status: res.StatusCode}, check.Secret()), nil
			}
			return setup.Works("Works. Acme accepted it."), nil
		}
	})
}

func vendor(probe func(*httptest.Server) setup.Probe) setuptest.Vendor {
	return setuptest.Vendor{
		App: "Acme",
		Requirements: []setup.Requirement{
			{Field: "url", Kind: setup.KindURL},
			{Field: "token", Kind: setup.KindSecret, Checkable: true},
			{Field: "signing_secret", Kind: setup.KindSecret, CheckNote: "Acme proves it."},
		},
		Probe:  probe,
		Values: func(srv *httptest.Server) map[string]string { return map[string]string{"url": srv.URL} },
		Accept: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
		Refuse: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		Outage: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
	}
}

// THE CONTROL: an honest vendor passes every clause.
func TestAnHonestProbePassesTheSuite(t *testing.T) {
	t.Parallel()
	setuptest.Run(t, honest())
}

// AND EVERY WAY OF LYING IS CAUGHT by the clause it breaks. A suite that
// agreed only with itself would certify nothing.
func TestTheSuiteCatchesEveryLie(t *testing.T) {
	t.Parallel()
	lie := func(answer func(check setup.Check) (setup.Verdict, error)) setuptest.Vendor {
		h := honest()
		inner := h.Probe
		h.Probe = func(srv *httptest.Server) setup.Probe {
			honestProbe := inner(srv)
			return func(ctx context.Context, check setup.Check) (setup.Verdict, error) {
				if v, err := answer(check); err != nil || v.Outcome != "" {
					return v, err
				}
				return honestProbe(ctx, check)
			}
		}
		return h
	}
	for _, c := range []struct {
		name   string
		clause string
		vendor setuptest.Vendor
	}{
		{"a checkable field it cannot check", "every checkable field works against an accepting vendor",
			lie(func(setup.Check) (setup.Verdict, error) {
				return setup.Unchecked("Acme proves it later."), nil
			})},
		{"an outage called a refusal", "an outage is unreachable, never a refusal",
			lie(func(setup.Check) (setup.Verdict, error) {
				return setup.Refused("Acme refused it."), nil
			})},
		{"a dead vendor called a refusal", "a vendor nothing can reach is unreachable",
			lie(func(setup.Check) (setup.Verdict, error) {
				return setup.Refused("Acme refused it."), nil
			})},
		{"a refusal called working", "a refusing vendor is a refusal",
			lie(func(setup.Check) (setup.Verdict, error) {
				return setup.Works("Works."), nil
			})},
		{"a note answered by the probe", "a field it cannot check is refused as one",
			lie(func(check setup.Check) (setup.Verdict, error) {
				if check.Field == "signing_secret" {
					return setup.Unchecked("Acme proves it."), nil
				}
				return setup.Verdict{}, nil
			})},
		{"a detail repeating the credential", "every checkable field works against an accepting vendor",
			lie(func(check setup.Check) (setup.Verdict, error) {
				return setup.Works("Works with " + check.Secret() + "."), nil
			})},
		{"a detail that is not a sentence", "every checkable field works against an accepting vendor",
			lie(func(setup.Check) (setup.Verdict, error) {
				return setup.Works("works"), nil
			})},
		{"a check made with a field that is not on the list",
			"a check made with other fields names them on a checkable field",
			func() setuptest.Vendor {
				v := honest()
				v.Requirements[1].CheckWith = []string{"region"}
				return v
			}()},
		{"a check made with a field that has its own test",
			"a check made with other fields names them on a checkable field",
			func() setuptest.Vendor {
				v := honest()
				v.Requirements = append(v.Requirements,
					setup.Requirement{Field: "pair", Kind: setup.KindSecret, Checkable: true,
						CheckWith: []string{"url", "token"}})
				return v
			}()},
		{"a group on a field that is not checkable",
			"a check made with other fields names them on a checkable field",
			func() setuptest.Vendor {
				v := honest()
				v.Requirements[2].CheckWith = []string{"url"}
				return v
			}()},
		{"no checkable field at all", "every checkable field works against an accepting vendor",
			func() setuptest.Vendor {
				v := honest()
				v.Requirements = v.Requirements[:1]
				return v
			}()},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if !fails(t, c.clause, c.vendor) {
				t.Errorf("the clause %q passed a vendor that lies", c.clause)
			}
		})
	}
}

// fails drives one named clause against a recorder and reports whether it
// went red. A recorder rather than a subtest, because a failing subtest fails
// its parent whatever t.Run returns.
func fails(t *testing.T, clause string, v setuptest.Vendor) bool {
	t.Helper()
	for _, c := range setuptest.Cases() {
		if c.Name != clause {
			continue
		}
		rec := &recorder{}
		func() {
			defer func() { _ = recover() }()
			c.Fn(rec, v)
		}()
		return rec.failed
	}
	t.Fatalf("no clause named %q", clause)
	return false
}

type recorder struct{ failed bool }

func (*recorder) Helper() {}

func (r *recorder) Errorf(string, ...any) { r.failed = true }

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = true
	panic(fmt.Sprintf(format, args...))
}
