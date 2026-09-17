package configapi_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
)

// A write made by this build keeps what this build cannot represent.
//
// Every case seeds a document as a NEWER peer would have written it, carrying
// keys no field of this build decodes, at the root, inside a known block, on a
// seat, on a unit and on a seat inside a unit. It then writes through a route
// the way an operator or the dashboard does, and reads the stored bytes back,
// because the struct is exactly what cannot show the loss.

// newerPeerDoc is the fixture every case extends: two root seats, a unit with
// a member, an MCP server.
const newerPeerDoc = movableDoc

// newer marks a value only a newer build writes.
const newer = "from-a-newer-build"

// seedNewerPeer stores newerPeerDoc with unknown keys at every level a write
// could lose one, and a known goal on the CTO.
func seedNewerPeer(t *testing.T, s *surface) string {
	t.Helper()
	return s.seedStored(t, newerPeerDoc, func(document map[string]any) {
		document["root_setting_"+newer] = map[string]any{"depth": json.Number("9007199254740993")}
		providers := document["providers"].(map[string]any)
		zulu := providers["llm"].(map[string]any)["zulu"].(map[string]any)
		zulu["provider_setting_"+newer] = true
		roles := document["roles"].([]any)
		cto := roles[1].(map[string]any)
		cto["seat_setting_"+newer] = "cto"
		cto["goal"] = "a known goal"
		units := document["units"].([]any)
		engineering := units[0].(map[string]any)
		engineering["unit_setting_"+newer] = "engineering"
		sre := engineering["roles"].([]any)[0].(map[string]any)
		sre["seat_setting_"+newer] = "sre"
		server := document["mcp_servers"].([]any)[0].(map[string]any)
		server["server_setting_"+newer] = "tracker"
	})
}

// storedTree is the active revision's stored bytes as a tree.
func storedTree(t *testing.T, s *surface) map[string]any {
	t.Helper()
	var tree map[string]any
	decoder := json.NewDecoder(strings.NewReader(s.activeDocument(t)))
	decoder.UseNumber()
	if err := decoder.Decode(&tree); err != nil {
		t.Fatalf("decode the stored document: %v", err)
	}
	return tree
}

// seatByHandle finds a seat anywhere in a stored tree.
func seatByHandle(tree map[string]any, handle string) map[string]any {
	var found map[string]any
	visit := func(list any) {
		items, _ := list.([]any)
		for _, item := range items {
			seat, _ := item.(map[string]any)
			if seat["handle"] == handle && found == nil {
				found = seat
			}
		}
	}
	visit(tree["roles"])
	var units func(list any)
	units = func(list any) {
		items, _ := list.([]any)
		for _, item := range items {
			unit, _ := item.(map[string]any)
			visit(unit["roles"])
			units(unit["children"])
		}
	}
	units(tree["units"])
	return found
}

// assertNewerKeysKept checks every unknown key seedNewerPeer wrote.
func assertNewerKeysKept(t *testing.T, s *surface, route string) {
	t.Helper()
	tree := storedTree(t, s)
	root, _ := tree["root_setting_"+newer].(map[string]any)
	if root == nil || root["depth"] != json.Number("9007199254740993") {
		t.Errorf("%s: the root key was lost or its integer rounded: %v", route, tree["root_setting_"+newer])
	}
	zulu := tree["providers"].(map[string]any)["llm"].(map[string]any)["zulu"].(map[string]any)
	if zulu["provider_setting_"+newer] != true {
		t.Errorf("%s: a key inside a known block was lost: %v", route, zulu)
	}
	if cto := seatByHandle(tree, "cto"); cto == nil || cto["seat_setting_"+newer] != "cto" {
		t.Errorf("%s: the CTO's key was lost: %v", route, cto)
	}
	if sre := seatByHandle(tree, "sre"); sre == nil || sre["seat_setting_"+newer] != "sre" {
		t.Errorf("%s: the SRE's key was lost: %v", route, sre)
	}
	var engineering map[string]any
	for _, item := range tree["units"].([]any) {
		if unit := item.(map[string]any); unit["name"] == "Engineering" {
			engineering = unit
		}
	}
	if engineering == nil || engineering["unit_setting_"+newer] != "engineering" {
		t.Errorf("%s: the unit's key was lost: %v", route, engineering)
	}
	server := tree["mcp_servers"].([]any)[0].(map[string]any)
	if server["server_setting_"+newer] != "tracker" {
		t.Errorf("%s: the server's key was lost: %v", route, server)
	}
}

// read is GET /config as a tree an editor changes.
func read(t *testing.T, s *surface) map[string]any {
	t.Helper()
	res := s.do(t, http.MethodGet, "/config", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /config = %d: %s", res.Code, res.Body)
	}
	var document map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

// A PATCH REPLACING THE ROSTER KEEPS EVERY SEAT'S AND UNIT'S UNKNOWN KEYS.
//
// The builder's save: a merge patch naming `roles` and `units` whole, built
// from a read this build served without the keys. Array replacement is RFC
// 7396's rule, so without the carry every seat and unit lost them. The roster
// is also REORDERED, so a positional match would put the CTO's key on the CEO.
func TestAPatchReplacingTheRosterKeepsWhatThisBuildCannotRepresent(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	seedNewerPeer(t, s)

	document := read(t, s)
	roles := document["roles"].([]any)
	roles[0], roles[1] = roles[1], roles[0]
	patch, err := json.Marshal(map[string]any{"roles": roles, "units": document["units"]})
	if err != nil {
		t.Fatal(err)
	}
	patchOnly(t, s, string(patch), summaryHeader)

	assertNewerKeysKept(t, s, "PATCH replacing the roster")
	if ceo := seatByHandle(storedTree(t, s), "ceo"); ceo["seat_setting_"+newer] != nil {
		t.Errorf("the CTO's key reached the CEO, which took its position: %v", ceo)
	}
}

// AND A PATCH THAT NAMES NO ARRAY KEEPS THEM TOO. The struct's own `roles`
// was merged back over the stored document after the restore, which replaced
// the stored array on every patch, whatever it named.
func TestAPatchNamingNoArrayKeepsWhatThisBuildCannotRepresent(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	seedNewerPeer(t, s)
	patchOnly(t, s, `{"mission": "unrelated"}`, summaryHeader)
	assertNewerKeysKept(t, s, "PATCH naming no array")
}

// A PATCH KEEPS EVERY LIST IT DID NOT NAME EXACTLY AS IT WAS STORED, one whose
// members have no identity included.
//
// A schedule cannot be matched to a stored one (two of a seat's schedules
// can share a cron and a task), so when a write REPLACES a list of them, the
// fields a newer build wrote on each are gone, and the API reference says
// so. A patch that names no schedule replaced none. It still lost them: the
// merge wrote this build's encoding of the whole company back over the
// stored document, which replaced every list in it, and the carry could
// bring back only what it could match.
func TestAPatchKeepsEveryListItDidNotName(t *testing.T) {
	t.Parallel()
	schedule := func(owner string) []any {
		return []any{map[string]any{
			"name": "standup", "cron": "0 9 * * 1-5", "task": "Post the standup",
			"schedule_setting": owner,
		}}
	}
	seed := func(t *testing.T) *surface {
		s := newSurface(t, nil)
		s.seedStored(t, newerPeerDoc, func(document map[string]any) {
			document["units"].([]any)[0].(map[string]any)["schedules"] = schedule("unit")
			document["roles"].([]any)[1].(map[string]any)["schedules"] = schedule("cto")
		})
		return s
	}
	setting := func(t *testing.T, s *surface, owner string) any {
		t.Helper()
		tree := storedTree(t, s)
		var holder map[string]any
		if owner == "unit" {
			holder = tree["units"].([]any)[0].(map[string]any)
		} else {
			holder = seatByHandle(tree, owner)
		}
		schedules, _ := holder["schedules"].([]any)
		if len(schedules) != 1 {
			t.Fatalf("%s holds %d schedules, want its one: %v", owner, len(schedules), holder)
		}
		return schedules[0].(map[string]any)["schedule_setting"]
	}

	t.Run("a patch naming no list", func(t *testing.T) {
		t.Parallel()
		s := seed(t)
		patchOnly(t, s, `{"mission": "unrelated", "providers": {"llm": {"zulu": {"model": "claude-opus-5"}}}}`, summaryHeader)
		for _, owner := range []string{"unit", "cto"} {
			if got := setting(t, s, owner); got != owner {
				t.Errorf("the %s schedule's field became %v, want it kept", owner, got)
			}
		}
		zulu := storedTree(t, s)["providers"].(map[string]any)["llm"].(map[string]any)["zulu"].(map[string]any)
		if zulu["model"] != "claude-opus-5" {
			t.Errorf("the named model was not written: %v", zulu)
		}
	})

	// The contrast, which is the documented limit: a patch replacing the
	// roster replaced the seats' schedules, and what it did not name keeps
	// its own.
	t.Run("a patch naming the roster", func(t *testing.T) {
		t.Parallel()
		s := seed(t)
		document := read(t, s)
		patch, err := json.Marshal(map[string]any{"roles": document["roles"]})
		if err != nil {
			t.Fatal(err)
		}
		patchOnly(t, s, string(patch), summaryHeader)
		if got := setting(t, s, "unit"); got != "unit" {
			t.Errorf("the unit's schedule field became %v, want it kept", got)
		}
		if got := setting(t, s, "cto"); got != nil {
			t.Errorf("the CTO's replaced schedule kept %v, which nothing can match it by", got)
		}
	})
}

// A PATCH'S RESTORED ENCODING LANDS ONLY WHERE THE PATCH NAMED SOMETHING.
//
// The case above holds the rule to this build's schema, where a list whose
// members have no identity sits only inside a seat or a unit. This one holds
// it where it has to keep holding when that changes: inside a block the patch
// named one key of, the list beside that key is the stored one, and only the
// named key takes the encoding's value.
func TestAPatchWritesBackOnlyWhatItNamed(t *testing.T) {
	t.Parallel()
	const merged = `{"block": {"named": "from the merge", "beside": [{"k": 1, "future": "kept"}]},
	                 "untouched": [{"k": 2, "future": "kept"}], "gone": "by the merge"}`
	const restored = `{"block": {"named": "restored", "beside": [{"k": 1}]},
	                   "untouched": [{"k": 2}], "listed": [{"k": 3}], "omitted": null}`
	for _, tc := range []struct {
		name, patch, want string
	}{{
		name:  "a key inside a block",
		patch: `{"block": {"named": "sent"}}`,
		want: `{"block": {"named": "restored", "beside": [{"k": 1, "future": "kept"}]},
		        "untouched": [{"k": 2, "future": "kept"}], "gone": "by the merge"}`,
	}, {
		name:  "a whole block",
		patch: `{"block": "sent whole"}`,
		want: `{"block": {"named": "restored", "beside": [{"k": 1}]},
		        "untouched": [{"k": 2, "future": "kept"}], "gone": "by the merge"}`,
	}, {
		// Null in the encoding, or no key at all, leaves what the merge
		// made of it: a merge patch writing the encoding back never
		// deleted anything by it either.
		name:  "keys the encoding holds as null or omits",
		patch: `{"omitted": "sent", "gone": null}`,
		want: `{"block": {"named": "from the merge", "beside": [{"k": 1, "future": "kept"}]},
		        "untouched": [{"k": 2, "future": "kept"}], "gone": "by the merge"}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var patch map[string]any
			if err := json.Unmarshal([]byte(tc.patch), &patch); err != nil {
				t.Fatal(err)
			}
			got, err := configapi.WriteBackNamed([]byte(merged), []byte(restored), patch)
			if err != nil {
				t.Fatalf("WriteBackNamed: %v", err)
			}
			var gotTree, wantTree any
			if err := json.Unmarshal(got, &gotTree); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantTree); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotTree, wantTree) {
				t.Errorf("wrote back\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

// A FULL PUT OF THE READ, WITH A SEAT MOVED, KEEPS THEM. Nobody sending a
// document through this build could have named one of these keys, so nobody
// meant to remove one; the SRE moves to the root and keeps its own.
func TestAPutKeepsWhatThisBuildCannotRepresent(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	seedNewerPeer(t, s)

	document := read(t, s)
	units := document["units"].([]any)
	engineering := units[0].(map[string]any)
	sre := engineering["roles"].([]any)[0]
	delete(engineering, "roles")
	document["roles"] = append(document["roles"].([]any), sre)
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config", string(body), summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	assertNewerKeysKept(t, s, "PUT with a seat moved")
}

// A KEY THIS BUILD KNOWS IS NOT CARRIED. The CTO's goal is left out of the
// write, which is how a person removes it, and it stays removed.
func TestAKnownFieldAWriteLeavesOutIsRemoved(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	seedNewerPeer(t, s)

	document := read(t, s)
	cto := document["roles"].([]any)[1].(map[string]any)
	delete(cto, "goal")
	patch, err := json.Marshal(map[string]any{"roles": document["roles"]})
	if err != nil {
		t.Fatal(err)
	}
	patchOnly(t, s, string(patch), summaryHeader)
	if got := seatByHandle(storedTree(t, s), "cto")["goal"]; got != nil {
		t.Errorf("the goal the write removed came back: %v", got)
	}
}

// A PATCH NAMING A KEY THIS BUILD CANNOT REPRESENT IS REFUSED, WHATEVER IT
// SETS IT TO, `null` INCLUDED.
//
// `null` in a merge patch deletes the key it names, so a patch naming an
// unknown key with one leaves no trace of it in the merged document: it is
// the one spelling the merged document cannot show. Answered 201 it was the
// worst outcome this route has, and the one its own hint rules out: the key
// is carried straight back, so the caller is told their deletion landed and
// nothing changed. A field only a newer build knows is removed from a node
// that knows it.
func TestAPatchNamingAKeyThisBuildCannotRepresentIsRefused(t *testing.T) {
	t.Parallel()
	// EXACTLY ONE UNKNOWN KEY PER CASE, written where the patch names it.
	// With a second one anywhere in the stored document the merged document
	// would carry that one and be refused for it, and every case here would
	// pass without the rule it is about ever running.
	//
	// The two array cases are the shape where the merged document DOES show
	// the key: a merge patch replaces a list wholesale, so a `null` inside a
	// replacing member is an ordinary key of that member rather than a
	// deletion. They are here because the rule is about the patch and not
	// about where the key sits.
	for name, tc := range map[string]struct {
		seed  func(map[string]any)
		patch string
	}{
		"deleting one at the root": {
			func(d map[string]any) { d["root_setting"] = "from-a-newer-build" },
			`{"root_setting": null}`,
		},
		"setting one at the root": {
			func(d map[string]any) { d["root_setting"] = "from-a-newer-build" },
			`{"root_setting": "mine now"}`,
		},
		"deleting one on a seat": {
			func(d map[string]any) {
				d["roles"].([]any)[1].(map[string]any)["seat_setting"] = "cto"
			},
			`{"roles": [{"name": "CEO", "handle": "ceo", "llm": "zulu"},
			            {"name": "CTO", "handle": "cto", "llm": "zulu", "seat_setting": null}]}`,
		},
		"deleting one inside a known block": {
			func(d map[string]any) {
				providers := d["providers"].(map[string]any)
				providers["llm"].(map[string]any)["zulu"].(map[string]any)["provider_setting"] = true
			},
			`{"providers": {"llm": {"zulu": {"provider_setting": null}}}}`,
		},
		"deleting one on an mcp server": {
			func(d map[string]any) {
				d["mcp_servers"].([]any)[0].(map[string]any)["server_setting"] = "tracker"
			},
			`{"mcp_servers": [{"name": "tracker", "command": "tracker-mcp", "server_setting": null}]}`,
		},
		// A typo is the same shape: the key is in no build, so deleting it
		// removes nothing and the merged document is clean either way.
		"deleting one it invented": {func(map[string]any) {}, `{"missionn": null}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t, nil)
			s.seedStored(t, newerPeerDoc, tc.seed)
			before := s.activeDocument(t)

			res := s.do(t, http.MethodPatch, "/config", tc.patch, summaryHeader)
			if res.Code != http.StatusBadRequest || decode(t, res)["error"] != "invalid_patch" {
				t.Fatalf("%s = %d, want 400 invalid_patch: %s", name, res.Code, res.Body)
			}
			if after := s.activeDocument(t); after != before {
				t.Errorf("a refused patch changed the stored document:\n%s\n%s", before, after)
			}
		})
	}

	// THE CONTRAST, or the rule above would read as "a patch cannot delete":
	// `null` still removes a section this build knows, which is what RFC
	// 7396's null is for and the only way to remove one through this route.
	t.Run("a key this build knows is deleted by the same gesture", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		seedNewerPeer(t, s)
		patchOnly(t, s, `{"integrations": {"gitlab": null}}`, summaryHeader)
		integrations, _ := storedTree(t, s)["integrations"].(map[string]any)
		if _, still := integrations["gitlab"]; still {
			t.Errorf("the section the patch deleted is still stored: %v", integrations)
		}
		assertNewerKeysKept(t, s, "a patch deleting a known section")
	})
}

// EVERY ENTITY WRITE KEEPS THEM, on the entity it replaced and everywhere
// else. It used to store this build's whole struct, so a per-seat edit from
// /setup, which is the engine's own most frequent write, erased every newer
// field in the company.
func TestAnEntityWriteKeepsWhatThisBuildCannotRepresent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, id string }{
		{configapi.EntityRoles, "cto"},
		{configapi.EntityRoles, "sre"},
		{configapi.EntityUnits, "Engineering"},
		{configapi.EntityLLMProviders, "zulu"},
		{configapi.EntityMCPServers, "tracker"},
	} {
		t.Run(tc.kind+"/"+tc.id, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t, nil)
			seedNewerPeer(t, s)

			entity := s.do(t, http.MethodGet, "/config/"+tc.kind+"/"+tc.id, "", nil)
			if entity.Code != http.StatusOK {
				t.Fatalf("GET = %d: %s", entity.Code, entity.Body)
			}
			res := s.do(t, http.MethodPut, "/config/"+tc.kind+"/"+tc.id, entity.Body.String(), summaryHeader)
			if res.Code != http.StatusCreated {
				t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
			}
			assertNewerKeysKept(t, s, "PUT /config/"+tc.kind+"/"+tc.id)
		})
	}
}

// RELOAD AND REVERT STORE THE DOCUMENT THEY RE-ACTIVATE, BYTE FOR BYTE AS A
// TREE. A reload is the credential-rotation gesture, made because nothing in
// the document changed, and a revert carries the old document: neither may
// store this build's re-encoding of it.
func TestReloadAndRevertKeepWhatThisBuildCannotRepresent(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	old := seedNewerPeer(t, s)

	if res := s.do(t, http.MethodPost, "/config/reload", "", nil); res.Code != http.StatusCreated {
		t.Fatalf("reload = %d: %s", res.Code, res.Body)
	}
	assertNewerKeysKept(t, s, "reload")

	// Moved on through the API, so the fleet pointer and the store agree on
	// what the revert is built over.
	if res := s.do(t, http.MethodPut, "/config", companyDoc, summaryHeader); res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d: %s", res.Code, res.Body)
	}
	if res := s.do(t, http.MethodPost, "/config/revisions/"+old+"/revert", "", nil); res.Code != http.StatusCreated {
		t.Fatalf("revert = %d: %s", res.Code, res.Body)
	}
	assertNewerKeysKept(t, s, "revert")
}

// A QUOTED ENTITY-TAG IS AN ENTITY WRITE'S PRECONDITION TOO. ApplyEntity
// compared the raw expectation with the revision id, so the standard spelling
// a caller forwards from an `If-Match` header was answered as a lost race.
func TestAnEntityWriteAcceptsEitherSpellingOfThePrecondition(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)
	for name, spell := range map[string]func(string) string{
		"a bare revision id":  func(id string) string { return id },
		"a quoted entity-tag": func(id string) string { return `"` + id + `"` },
		"the wildcard":        func(string) string { return "*" },
	} {
		current, found, err := s.configs.Active(t.Context())
		if err != nil || !found {
			t.Fatalf("active: %v", err)
		}
		if _, err := s.svc.ApplyEntity(t.Context(), configapi.ApplyEntityRequest{
			Kind: configapi.EntityRoles, ID: "cto",
			Body:    []byte(`{"name": "CTO", "handle": "cto", "llm": "zulu", "goal": "` + name + `"}`),
			Summary: "an entity", Operator: "operator", Expect: spell(current.ID),
		}); err != nil {
			t.Errorf("ApplyEntity with %s = %v", name, err)
		}
	}
}
