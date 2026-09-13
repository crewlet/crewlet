package configapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// movableDoc holds a root seat with a literal per-seat credential and a unit
// to move it into.
const movableDoc = companyDoc + `
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false}
units:
  - name: Engineering
    roles:
      - {name: SRE, handle: sre, llm: zulu}
`

// A SEAT MOVED THROUGH THE WRITE SURFACE KEEPS ITS CREDENTIAL.
//
// The whole loop a builder runs: read the redacted document, move a seat from
// the root into a unit, send it back. The restore used to match a seat only
// within the list it now sat in, so the move was refused with a redaction
// error on a credential the caller never touched.
func TestAMovedSeatKeepsItsCredentialThroughAPut(t *testing.T) {
	t.Parallel()
	const literal = "cto-tracker-literal"
	doc := strings.Replace(movableDoc, "    handle: cto\n    llm: zulu\n",
		"    handle: cto\n    llm: zulu\n    mcp_env: {tracker: {TOKEN: "+literal+"}}\n", 1)
	s := newSurface(t, nil)
	s.seed(t, doc, nil)

	read := s.do(t, http.MethodGet, "/config", "", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("GET /config = %d: %s", read.Code, read.Body)
	}
	var document map[string]any
	if err := json.Unmarshal(read.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	roles, _ := document["roles"].([]any)
	cto := roles[1]
	document["roles"] = roles[:1]
	units, _ := document["units"].([]any)
	engineering, _ := units[0].(map[string]any)
	members, _ := engineering["roles"].([]any)
	engineering["roles"] = append(members, cto)
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "__redacted__") {
		t.Fatalf("the moved seat carries no mask, so this proves nothing: %s", body)
	}

	res := s.do(t, http.MethodPut, "/config", string(body), map[string]string{"X-Summary": "move the CTO"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	stored := s.activeDocument(t)
	if !strings.Contains(stored, literal) || strings.Contains(stored, "__redacted__") {
		t.Errorf("the moved seat's credential was not restored: %s", stored)
	}
}

// A DOCUMENT READ AND SENT BACK UNCHANGED KEEPS A DISABLED SCHEDULE DISABLED.
//
// The read once dropped every toggle's state, so a schedule kept in config
// with `enabled: false` was served without the field and the unchanged
// document sent back stored a schedule that fires. The assertion is on the
// read as well as the stored result, because the builder edits exactly what
// the read serves.
func TestAReadDocumentSentBackKeepsADisabledSchedule(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(companyDoc, "    handle: ceo\n    llm: zulu\n",
		"    handle: ceo\n    llm: zulu\n    schedules:\n"+
			"      - {name: standup, cron: \"0 9 * * 1-5\", task: Post the standup, enabled: false}\n", 1)
	s := newSurface(t, nil)
	s.seed(t, doc, nil)

	read := s.do(t, http.MethodGet, "/config", "", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("GET /config = %d: %s", read.Code, read.Body)
	}
	if !strings.Contains(read.Body.String(), `"enabled":false`) {
		t.Fatalf("the read dropped the disabled schedule's toggle: %s", read.Body)
	}

	res := s.do(t, http.MethodPut, "/config", read.Body.String(),
		map[string]string{"X-Summary": "send it back"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	if stored := s.activeDocument(t); !strings.Contains(stored, `"enabled":false`) {
		t.Errorf("sending the read back enabled the schedule: %s", stored)
	}
}
