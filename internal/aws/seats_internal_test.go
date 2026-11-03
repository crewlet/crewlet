package aws

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sigv4"
)

// A SEAT'S NAME FITS AWS AND IS ITS OWN: the longest company and handle still
// fit IAM's role name and Roles Anywhere's certificate name, and two handles
// alike for twenty characters never share a name, since a seat role's trust
// pins its certificate name.
func TestASeatsNameFitsAWSAndIsItsOwn(t *testing.T) {
	t.Parallel()
	company := Company(strings.Repeat("c", 20))
	long := strings.Repeat("a", 64)
	for _, handle := range []string{"sre", long, long[:63] + "b", "a-" + strings.Repeat("-", 30) + "z"} {
		slug := SeatSlug(handle)
		if slug != SeatSlug(handle) {
			t.Fatalf("%q: the slug is not deterministic", handle)
		}
		if n := len(company.SeatRoleName(slug)); n > 64 {
			t.Errorf("%q: role name is %d characters", handle, n)
		}
		if cn := seatCN(company, slug); !cnPattern.MatchString(cn) {
			t.Errorf("%q: certificate name %q is outside %s", handle, cn, cnPattern)
		}
		if got, ok := company.slugOf(company.SeatRoleName(slug)); !ok || got != slug {
			t.Errorf("%q: the role name does not read back as its slug", handle)
		}
	}
	if SeatSlug(long) == SeatSlug(long[:63]+"b") {
		t.Error("two handles alike for twenty characters share a name")
	}
	if got := SeatSlug("sre"); !strings.HasPrefix(got, "sre-") || len(got) != len("sre-")+seatHashChars {
		t.Errorf("SeatSlug(sre) = %q", got)
	}
	if _, ok := Company("acme").slugOf("crewlet-other-sre-0a1b2c"); ok {
		t.Error("another company's role read as a seat")
	}
}

// EVERY ACCESS READS BACK FROM THE POLICIES THAT GRANT IT, and a set that is
// no level's, or a policy that is not Crewlet's, says so.
func TestAccessReadsBackFromItsPolicies(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	for _, level := range config.AWSLevels() {
		for _, iamRead := range []bool{false, true} {
			want := Access{Level: level, IAMRead: iamRead}
			if got, foreign := conn.AccessOf(conn.Policies(want)); got != want || foreign != nil {
				t.Errorf("%+v read back as %+v (foreign %v)", want, got, foreign)
			}
		}
	}
	off := conn.Policies(Access{Level: config.AWSLevelOperate, Off: true})
	if len(off) != 1 || !strings.HasSuffix(off[0], ":iam::aws:policy/AWSDenyAll") {
		t.Fatalf("an off role holds %v, want AWS's deny-all alone", off)
	}
	if got, _ := conn.AccessOf(off); !got.Off || got.Level != "" {
		t.Errorf("an off role reads as %+v", got)
	}
	gap := []string{conn.resolve(policyARN(levelOperate))}
	foreignARN := "arn:aws:iam::aws:policy/AdministratorAccess"
	if got, foreign := conn.AccessOf(append(gap, foreignARN)); got.Level != "" || !slices.Equal(foreign, []string{foreignARN}) {
		t.Errorf("Operate without Read and a foreign grant read as %+v, foreign %v", got, foreign)
	}
}

// CHANGES TAKE THE ONE SAFE ORDER: every attach before any detach, AWS's
// deny-all lifted last, so off denies before it detaches, on grants before it
// lifts the deny, and a level never lapses mid-way. A foreign grant is left.
func TestChangesAttachFirstAndLiftTheDenyLast(t *testing.T) {
	t.Parallel()
	conn := mustConnection(t)
	p := func(name string) string { return conn.resolve(policyARN(name)) }
	deny := conn.resolve(denyAllARN)
	foreign := "arn:aws:iam::123456789012:policy/someone-elses"
	read, operate := Access{Level: config.AWSLevelRead}, Access{Level: config.AWSLevelOperate}
	for name, tc := range map[string]struct {
		have []string
		want Access
		out  []Change
	}{
		"create at operate": {nil, operate, []Change{{true, p(levelRead)}, {true, p(levelOperate)}}},
		"raise":             {conn.Policies(read), operate, []Change{{true, p(levelOperate)}}},
		"lower":             {conn.Policies(operate), read, []Change{{false, p(levelOperate)}}},
		"off": {append(conn.Policies(Access{Level: config.AWSLevelOperate, IAMRead: true}), foreign),
			Access{Level: config.AWSLevelOperate, Off: true},
			[]Change{{true, deny}, {false, p(levelRead)}, {false, p(levelOperate)}, {false, p(levelIAMRead)}}},
		"on":          {[]string{deny}, read, []Change{{true, p(levelRead)}, {false, deny}}},
		"iam read on": {conn.Policies(read), Access{Level: config.AWSLevelRead, IAMRead: true}, []Change{{true, p(levelIAMRead)}}},
		"in place":    {conn.Policies(operate), operate, nil},
	} {
		if got := conn.Changes(tc.have, tc.want); !slices.Equal(got, tc.out) {
			t.Errorf("%s: %v, want %v", name, got, tc.out)
		}
	}
}

// fakeIAM answers Roles Anywhere's CreateSession for the provisioner, then
// each IAM call through answer, recording every action and its form.
type fakeIAM struct {
	mu     sync.Mutex
	calls  []url.Values
	answer func(form url.Values) (int, string)
	// grants decides each CreateSession by the leaf's name and the profile
	// asked through; nil grants every one.
	grants func(cn, profile string) bool
}

func (f *fakeIAM) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, form := range f.calls {
		out = append(out, form.Get("Action"))
	}
	return out
}

func (f *fakeIAM) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/sessions" {
			der, _ := base64.StdEncoding.DecodeString(r.Header.Get(sigv4.HeaderX509))
			leaf, _ := x509.ParseCertificate(der)
			var in struct {
				Profile string `json:"profileArn"`
			}
			_ = json.Unmarshal(body, &in)
			if f.grants != nil && (leaf == nil || !f.grants(leaf.Subject.CommonName, in.Profile)) {
				w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"credentialSet": []any{map[string]any{
				"credentials": map[string]any{"accessKeyId": "ASIAPROVEXAMPLE00001", "secretAccessKey": "s",
					"sessionToken": "prov-token", "expiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
			}}})
			return
		}
		form, _ := url.ParseQuery(string(body))
		f.mu.Lock()
		f.calls = append(f.calls, form)
		f.mu.Unlock()
		status, out := f.answer(form)
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	c := newClient(mustConnection(t), srv.Client(), srv.URL)
	c.ra.wait = func(int) time.Duration { return time.Millisecond }
	return c
}

func iamOK(action, result string) (int, string) {
	return http.StatusOK, fmt.Sprintf("<%[1]sResponse><%[1]sResult>%[2]s</%[1]sResult></%[1]sResponse>", action, result)
}

func iamError(status int, code string) (int, string) {
	return status, "<ErrorResponse><Error><Type>Sender</Type><Code>" + code +
		"</Code><Message>refused</Message></Error><RequestId>r</RequestId></ErrorResponse>"
}

func roleXML(name, trust string) string {
	return "<Role><RoleName>" + name + "</RoleName><Path>/crewlet/acme/seat/</Path>" +
		"<Arn>arn:aws:iam::123456789012:role/crewlet/acme/seat/" + name + "</Arn><RoleId>AROAEXAMPLE</RoleId>" +
		"<CreateDate>2026-11-03T00:00:00Z</CreateDate><AssumeRolePolicyDocument>" + url.PathEscape(trust) +
		"</AssumeRolePolicyDocument><PermissionsBoundary><PermissionsBoundaryType>Policy</PermissionsBoundaryType>" +
		"<PermissionsBoundaryArn>arn:aws:iam::123456789012:policy/crewlet/acme/crewlet-acme-seat-boundary</PermissionsBoundaryArn>" +
		"</PermissionsBoundary></Role>"
}

func seatTrust(t *testing.T, slug string) string {
	t.Helper()
	conn := mustConnection(t)
	return string(compactJSON(concrete(seatTrustDoc(seatCN(conn.Company, slug)), conn.vars())))
}

// THE LISTING DECIDES EXISTENCE, across every page and under the seat path.
func TestSeatRolesListsEveryPageUnderTheSeatPath(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{answer: func(form url.Values) (int, string) {
		if form.Get("PathPrefix") != "/crewlet/acme/seat/" {
			t.Errorf("ListRoles under %q", form.Get("PathPrefix"))
		}
		if form.Get("Marker") == "" {
			return iamOK("ListRoles", "<Roles><member><RoleName>crewlet-acme-a-000000</RoleName><Arn>arn:a</Arn></member></Roles>"+
				"<IsTruncated>true</IsTruncated><Marker>m1</Marker>")
		}
		return iamOK("ListRoles", "<Roles><member><RoleName>crewlet-acme-b-000000</RoleName><Arn>arn:b</Arn></member></Roles>"+
			"<IsTruncated>false</IsTruncated>")
	}}
	got, err := f.client(t).SeatRoles(context.Background())
	if err != nil || len(got) != 2 || got["crewlet-acme-a-000000"] != "arn:a" || got["crewlet-acme-b-000000"] != "arn:b" {
		t.Fatalf("%v %v", got, err)
	}
}

// A SEAT ROLE IS CREATED CONTAINED: under the seat path, within the boundary,
// trusting the broker only in the seat's own name, for fifteen minutes at a
// time, tagged as Crewlet's, and holding nothing yet.
func TestCreateSeatRoleIsContainedFromTheStart(t *testing.T) {
	t.Parallel()
	slug := SeatSlug("sre")
	f := &fakeIAM{}
	f.answer = func(form url.Values) (int, string) {
		return iamOK("CreateRole", roleXML(form.Get("RoleName"), form.Get("AssumeRolePolicyDocument")))
	}
	role, err := f.client(t).CreateSeatRole(context.Background(), "sre", slug)
	if err != nil {
		t.Fatal(err)
	}
	form := f.calls[0]
	want := map[string]string{
		"Action": "CreateRole", "RoleName": "crewlet-acme-" + slug, "Path": "/crewlet/acme/seat/",
		"PermissionsBoundary": "arn:aws:iam::123456789012:policy/crewlet/acme/crewlet-acme-seat-boundary",
		"MaxSessionDuration":  "3600", "Tags.member.3.Key": "crewlet:seat", "Tags.member.3.Value": slug,
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
	if ok, hint := samePolicy(form.Get("AssumeRolePolicyDocument"), concrete(seatTrustDoc("crewlet-acme-seat-"+slug),
		mustConnection(t).vars())); !ok || !strings.Contains(form.Get("AssumeRolePolicyDocument"), `"CN=crewlet-acme-seat-`+slug+`"`) {
		t.Errorf("the trust is not pinned to the seat's certificate name: %s", hint)
	}
	if role.ID != "AROAEXAMPLE" || len(f.calls) != 1 {
		t.Errorf("role %+v after %v", role, f.actions())
	}
}

// NOTHING IS ADOPTED BY ITS NAME: a role already there is reported for the
// caller to judge from its own record, and never read back as made.
func TestCreateSeatRoleNeverAdoptsByName(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{answer: func(url.Values) (int, string) { return iamError(http.StatusConflict, "EntityAlreadyExists") }}
	_, err := f.client(t).CreateSeatRole(context.Background(), "sre", SeatSlug("sre"))
	if !errors.Is(err, ErrRoleExists) || !slices.Equal(f.actions(), []string{"CreateRole"}) {
		t.Errorf("%v after %v", err, f.actions())
	}
}

// REMOVING DETACHES THEN DELETES; a role no longer listed is done without a
// call that IAM would refuse, and an inline grant keeps the role for a person.
func TestDeleteSeatRole(t *testing.T) {
	t.Parallel()
	slug := SeatSlug("sre")
	name := Company("acme").SeatRoleName(slug)
	for _, tc := range []struct {
		name    string
		listed  bool
		inline  string
		actions []string
		err     error
	}{
		{"gone", false, "", []string{"ListRoles"}, nil},
		{"held", true, "", []string{"ListRoles", "GetRole", "ListAttachedRolePolicies", "ListRolePolicies",
			"DetachRolePolicy", "DetachRolePolicy", "DeleteRole"}, nil},
		{"inline", true, "<member>hand-made</member>", []string{"ListRoles", "GetRole", "ListAttachedRolePolicies",
			"ListRolePolicies"}, ErrInlinePolicy},
	} {
		f := &fakeIAM{}
		f.answer = func(form url.Values) (int, string) {
			switch form.Get("Action") {
			case "ListRoles":
				roles := ""
				if tc.listed {
					roles = "<member><RoleName>" + name + "</RoleName><Arn>arn:r</Arn></member>"
				}
				return iamOK("ListRoles", "<Roles>"+roles+"</Roles><IsTruncated>false</IsTruncated>")
			case "GetRole":
				return iamOK("GetRole", roleXML(name, seatTrust(t, slug)))
			case "ListAttachedRolePolicies":
				return iamOK("ListAttachedRolePolicies", "<AttachedPolicies>"+
					"<member><PolicyArn>arn:aws:iam::aws:policy/AWSDenyAll</PolicyArn></member>"+
					"<member><PolicyArn>arn:aws:iam::123456789012:policy/crewlet/acme/crewlet-acme-level-read</PolicyArn></member>"+
					"</AttachedPolicies><IsTruncated>false</IsTruncated>")
			case "ListRolePolicies":
				return iamOK("ListRolePolicies", "<PolicyNames>"+tc.inline+"</PolicyNames><IsTruncated>false</IsTruncated>")
			case "DetachRolePolicy":
				return iamOK("DetachRolePolicy", "")
			}
			return iamOK("DeleteRole", "")
		}
		err := f.client(t).DeleteSeatRole(context.Background(), name)
		if !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if got := f.actions(); !slices.Equal(got, tc.actions) {
			t.Errorf("%s: calls %v, want %v", tc.name, got, tc.actions)
		}
	}
}
