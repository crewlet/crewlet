package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSigner appends to each body and signs it with a header.
type fakeSigner struct{ calls int }

func (f *fakeSigner) Sign(_ context.Context, req *http.Request, body []byte) ([]byte, error) {
	f.calls++
	signed := append(append([]byte(nil), body...), " signed"...)
	req.Header.Set("Authorization", "Signed "+string(signed))
	return signed, nil
}

// A SIGNED SERVER'S REQUESTS ARE SIGNED LAST AND ONLY ON ITS OWN ORIGIN: the
// signer sees every header the request carries and the body as sent, and a
// request anywhere else carries no signature.
func TestASignedServersRequestsAreSignedOnItsOwnOrigin(t *testing.T) {
	t.Parallel()
	var gotAuth, gotBody, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotBody, gotVersion = r.Header.Get("Authorization"), string(b), r.Header.Get(protocolVersionHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	signer := &fakeSigner{}
	ident := &httpIdentity{base: http.DefaultTransport, signer: signer, origin: originOf(srv.URL), server: "aws"}
	v := "2025-06-18"
	ident.setProtocolVersion(v)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`))
	resp, err := ident.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotBody != `{"jsonrpc":"2.0"} signed` || gotAuth != "Signed "+gotBody || gotVersion != v {
		t.Errorf("sent body %q, Authorization %q, version %q", gotBody, gotAuth, gotVersion)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer other.Close()
	req, _ = http.NewRequest(http.MethodPost, other.URL+"/mcp", strings.NewReader(`{}`))
	if resp, err := ident.RoundTrip(req); err == nil {
		_ = resp.Body.Close()
	}
	if gotAuth != "" || signer.calls != 1 {
		t.Errorf("a request to another origin was signed: %q after %d signs", gotAuth, signer.calls)
	}
	a, b := Spec{Name: "aws", Signer: signer}, Spec{Name: "aws", Signer: &fakeSigner{}}
	if a.equal(b) || !a.equal(Spec{Name: "aws", Signer: signer}) {
		t.Error("a signer is not compared by identity")
	}
}
