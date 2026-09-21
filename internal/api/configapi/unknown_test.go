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
// keys no field of this build decodes: at the root, inside a KEYED block
// (`providers.llm.zulu`) and on each member of a LIST whose members can name
// themselves (`mcp_servers`). It then writes through a route the way an
// operator or the dashboard does, and reads the stored bytes back, because the
// struct is exactly what cannot show the loss.
//
// THE LEVELS ARE THE ONES A SETTINGS REVISION HAS. They were a seat, a unit
// and a seat inside a unit as well — the three deepest, and the ones a
// positional match got wrong — and those are the org chart's own domain now,
// with its own records and its own write path. What makes `mcp_servers` the
// right stand-in is the property that mattered: its members are matched by
// IDENTITY rather than by position, so a reordered list is still the test it
// was.

// newerPeerDoc is the fixture every case extends: two providers and two MCP
// servers, so a reorder has something to get wrong.
const newerPeerDoc = entityDoc

// newer marks a value only a newer build writes.
const newer = "from-a-newer-build"

// seedNewerPeer stores newerPeerDoc with unknown keys at every level a write
// could lose one, and a known field on the tracker server.
func seedNewerPeer(t *testing.T, s *surface) string {
	t.Helper()
	return s.seedStored(t, newerPeerDoc, func(document map[string]any) {
		document["root_setting_"+newer] = map[string]any{"depth": json.Number("9007199254740993")}
		providers := document["providers"].(map[string]any)
		zulu := providers["llm"].(map[string]any)["zulu"].(map[string]any)
		zulu["provider_setting_"+newer] = true
		servers := document["mcp_servers"].([]any)
		tracker := servers[0].(map[string]any)
		tracker["server_setting_"+newer] = "tracker"
		tracker["url"] = "https://mcp.example.com"
		notion := servers[1].(map[string]any)
		notion["server_setting_"+newer] = "notion"
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

// serverByName finds an MCP server in a stored tree.
func serverByName(tree map[string]any, name string) map[string]any {
	items, _ := tree["mcp_servers"].([]any)
	for _, item := range items {
		server, _ := item.(map[string]any)
		if server["name"] == name {
			return server
		}
	}
	return nil
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
	// BY NAME, never by position, which is the whole reason a list member
	// that can name itself is matched the way it is: the reorder case below
	// would otherwise pass while putting one server's key on the other.
	for _, name := range []string{"tracker", "notion"} {
		server := serverByName(tree, name)
		if server == nil || server["server_setting_"+newer] != name {
			t.Errorf("%s: %s's key was lost: %v", route, name, server)
		}
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

// A PATCH REPLACING A WHOLE LIST KEEPS EVERY MEMBER'S UNKNOWN KEYS.
//
// The builder's save: a merge patch naming `mcp_servers` whole, built from a
// read this build served without the keys. Array replacement is RFC 7396's
// rule, so without the carry every member lost them. The list is also
// REORDERED, so a positional match would put one server's key on the other.
func TestAPatchReplacingAListKeepsWhatThisBuildCannotRepresent(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	seedNewerPeer(t, s)

	document := read(t, s)
	servers := document["mcp_servers"].([]any)
	servers[0], servers[1] = servers[1], servers[0]
	patch, err := json.Marshal(map[string]any{"mcp_servers": servers})
	if err != nil {
		t.Fatal(err)
	}
	patchOnly(t, s, string(patch), summaryHeader)

	// assertNewerKeysKept matches BY NAME, so it is what catches a key that
	// followed a position rather than its owner.
	assertNewerKeysKept(t, s, "PATCH replacing a list")
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

// A PATCH KEEPS EVERY LIST IT DID NOT NAME EXACTLY AS IT WAS STORED.
//
// The merge wrote this build's encoding of the WHOLE company back over the
// stored document, which replaced every list in it whatever the patch named,
// and the carry could bring back only what it could match.
//
// # The list this is shown on, and the limit that used to be shown beside it
//
// The pair of cases used to be a seat's `schedules` — a list whose members
// have NO identity, so a replacement genuinely loses what a newer build wrote
// on each, which the API reference states. Every list of that shape sat on a
// seat or a unit, and those are the org chart's own domain now; the settings a
// revision still holds have no unidentified list left to show it on. The rule
// itself did not move, and [TestAPatchWritesBackOnlyWhatItNamed] is where it
// is held, against a schema written for the purpose rather than against
// whichever fields this build happens to have today.
//
// What is shown here is the half that is about the MERGE rather than about
// matching: a patch naming one key of one block must leave every list it never
// mentioned exactly as the bytes hold it.
func TestAPatchKeepsEveryListItDidNotName(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	seedNewerPeer(t, s)

	patchOnly(t, s, `{"mission": "unrelated", "providers": {"llm": {"zulu": {"model": "claude-opus-5"}}}}`,
		summaryHeader)

	// THE LIST THE PATCH NEVER MENTIONED, with the keys a newer build wrote
	// on each of its members still on the right member.
	assertNewerKeysKept(t, s, "PATCH naming one key of one block")
	zulu := storedTree(t, s)["providers"].(map[string]any)["llm"].(map[string]any)["zulu"].(map[string]any)
	if zulu["model"] != "claude-opus-5" {
		t.Errorf("the named model was not written: %v", zulu)
	}
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

// A FULL PUT OF THE READ, WITH A LIST MEMBER MOVED, KEEPS THEM. Nobody
// sending a document through this build could have named one of these keys,
// so nobody meant to remove one; the servers swap places and each keeps its
// own.
func TestAPutKeepsWhatThisBuildCannotRepresent(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	seedNewerPeer(t, s)

	document := read(t, s)
	servers := document["mcp_servers"].([]any)
	servers[0], servers[1] = servers[1], servers[0]
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config", string(body), summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	assertNewerKeysKept(t, s, "PUT with a list member moved")
}

// A KEY THIS BUILD KNOWS IS NOT CARRIED. The tracker server's tool prefix is
// left out of the write, which is how a person removes it, and it stays
// removed.
func TestAKnownFieldAWriteLeavesOutIsRemoved(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seedStored(t, newerPeerDoc, func(document map[string]any) {
		document["mcp_servers"].([]any)[0].(map[string]any)["tool_prefix"] = "tr_"
	})

	document := read(t, s)
	for _, item := range document["mcp_servers"].([]any) {
		server := item.(map[string]any)
		if server["name"] == "tracker" {
			delete(server, "tool_prefix")
		}
	}
	patch, err := json.Marshal(map[string]any{"mcp_servers": document["mcp_servers"]})
	if err != nil {
		t.Fatal(err)
	}
	patchOnly(t, s, string(patch), summaryHeader)
	if got := serverByName(storedTree(t, s), "tracker")["tool_prefix"]; got != nil {
		t.Errorf("the tool prefix the write removed came back: %v", got)
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
			`{"mcp_servers": [{"name": "tracker", "transport": "http",
			  "url": "https://mcp.example.com", "server_setting": null}]}`,
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
		{configapi.EntityLLMProviders, "zulu"},
		{configapi.EntityLLMProviders, "yankee"},
		{configapi.EntityMCPServers, "tracker"},
		{configapi.EntityMCPServers, "notion"},
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
			Kind: configapi.EntityLLMProviders, ID: "zulu",
			Body: []byte(`{"type": "anthropic", "model": "claude-sonnet-5",
			  "api_keys": ["sk-` + strings.ReplaceAll(name, " ", "-") + `"]}`),
			Summary: "an entity", Operator: "operator", Expect: spell(current.ID),
		}); err != nil {
			t.Errorf("ApplyEntity with %s = %v", name, err)
		}
	}
}
