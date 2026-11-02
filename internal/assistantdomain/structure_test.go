package assistantdomain_test

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE LARGEST RECORD FITS ITS DECLARATION (spike S4).
//
// The domain declares the largest record it publishes, and the declaration is
// a LIMIT: the publisher refuses a record past it `record_too_large` after a
// turn already showed the person what it was about to store. So the worst
// record each bound admits is built here, through the real sealer and encoder,
// and held under the declaration; and the declaration is held within the
// transport's own ceiling and above a quarter of its worst record, so the gate
// reserve it sizes is not room no append could use. Mutation: raise
// ProposalBodyMaxBytes past what the declaration holds and this goes red.
func TestTheLargestRecordFitsItsDeclaration(t *testing.T) {
	t.Parallel()
	sealer := newSealer(t)
	conversation, record := uuid.New(), uuid.New()
	seal := func(n int, column string) string {
		t.Helper()
		plain := bytes.Repeat([]byte{'x'}, n)
		sealed, err := sealer.Seal(plain, assistantdomain.AAD(conversation,
			assistantdomain.RecordProposal, record, column))
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		return sealed
	}
	long := strings.Repeat("y", assistantdomain.TitleMaxBytes)
	worst := map[string]struct {
		subject assistantdomain.Subject
		op      assistantdomain.OpKind
		payload any
	}{
		"a proposal": {assistantdomain.ProposalSubject(conversation, record),
			assistantdomain.OpProposalCreated, assistantdomain.ProposalCreated{
				V: 1, Turn: uuid.NewString(), Owner: uuid.NewString(), Tool: long, Act: long,
				Scope: wire.ScopeServer, Method: "PATCH",
				Path:          "/" + strings.Repeat("p", assistantdomain.QueryMaxBytes-1),
				QuerySealed:   seal(assistantdomain.QueryMaxBytes, assistantdomain.ColumnQuery),
				BodySealed:    seal(assistantdomain.ProposalBodyMaxBytes, assistantdomain.ColumnBody),
				RequestDigest: hex64("a"), Digest: "sha256:" + hex64("d"), OpKey: op("approve"),
				SourceRevision: long, StepUp: true,
				CardSealed: seal(assistantdomain.CardMaxBytes, assistantdomain.ColumnCard),
				ExpiresAt:  monday,
			}},
		"a parked turn": {assistantdomain.TurnSubject(conversation, record),
			assistantdomain.OpTurnParked, assistantdomain.TurnParked{
				V: 1, MessagesSealed: seal(assistantdomain.ParkedMaxBytes, assistantdomain.ColumnMessages),
			}},
		"a card": {assistantdomain.TurnSubject(conversation, record),
			assistantdomain.OpCardRecorded, assistantdomain.CardRecorded{
				V: 1, Card: uuid.NewString(), Message: uuid.NewString(), Type: wire.CardTable,
				Apps:       slices.Repeat([]string{long}, assistantdomain.MaxApps),
				BodySealed: seal(assistantdomain.CardMaxBytes, assistantdomain.ColumnBody),
			}},
	}
	spec := assistantdomain.Domain{}.Stream()
	var largest int
	for name, w := range worst {
		body, err := json.Marshal(w.payload)
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		encoded, err := assistantdomain.Encode(assistantdomain.MutationRecord{
			RecordEnvelope: assistantdomain.RecordEnvelope{
				OpID: op(strings.Repeat("n", 80)), Subject: w.subject, Op: w.op,
				CreatedAt: monday, Gen: 1 << 31, Writer: long,
				Scope: assistantdomain.ScopeSet{Subject: true},
			},
			Mutation: body, Actor: long, ActorKind: iam.ActorHuman, OperatorID: long,
		})
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		if int64(len(encoded)) > spec.MaxRecordBytes {
			t.Errorf("the worst %s is %d bytes and the domain declares %d: the publisher "+
				"would refuse it after the person was shown it", name, len(encoded),
				spec.MaxRecordBytes)
		}
		t.Logf("the worst %s encodes to %d bytes", name, len(encoded))
		largest = max(largest, len(encoded))
	}
	if spec.MaxRecordBytes > statelog.MaxTransportRecordBytes {
		t.Errorf("the declaration %d is past what the transport carries, %d",
			spec.MaxRecordBytes, statelog.MaxTransportRecordBytes)
	}
	if spec.MaxRecordBytes > 4*int64(largest) {
		t.Errorf("the declaration %d is over four times the worst record, %d: the gate "+
			"reserve it sizes holds back room no append could use", spec.MaxRecordBytes, largest)
	}
}

// THE APPLIER NEVER OPENS A SEALED VALUE.
//
// Every node must write identical rows from one record whatever keys its ring
// holds, so the applier holds no keyring and calls nothing that opens one: its
// type has no field to hold a sealer, no apply file imports the keyring's
// package or names the sealer's methods, and a record whose sealed fields are
// not envelopes at all applies with them written through byte for byte.
// Mutation: give the applier a sealer and open a title in its create, and this
// names the file and the call.
func TestTheApplierNeverOpensASealedValue(t *testing.T) {
	t.Parallel()
	if fields := reflect.TypeFor[assistantdomain.Applier]().NumField(); fields != 0 {
		t.Errorf("the applier holds %d fields; it holds nothing, a keyring least of all", fields)
	}
	dir := filepath.Join(sourcetree.Root(t), "internal", "assistantdomain")
	files, err := filepath.Glob(filepath.Join(dir, "apply*.go"))
	if err != nil || len(files) < 4 {
		t.Fatalf("the walk found %d apply files (%v), so it is not reading the applier", len(files), err)
	}
	opening := []string{"Sealer", "Open", "openString", "Decrypt", "sealer", "Seal", "sealString"}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); strings.HasSuffix(p, "/internal/secrets") {
				t.Errorf("%s imports the keyring's package", filepath.Base(path))
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && slices.Contains(opening, id.Name) {
				t.Errorf("%s names %s at %s: the applier writes sealed values through "+
					"and never opens one", filepath.Base(path), id.Name, fset.Position(id.Pos()))
			}
			return true
		})
	}

	// AND IN BEHAVIOUR: values that are not envelopes at all, written through.
	n := newHarness(t)
	conversation := uuid.New()
	body, err := json.Marshal(assistantdomain.ConversationCreated{V: 1, Owner: uuid.NewString(),
		TitleSealed: "not an envelope, and nobody looks"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := assistantdomain.Encode(assistantdomain.MutationRecord{
		RecordEnvelope: assistantdomain.RecordEnvelope{
			OpID: op("raw"), Subject: assistantdomain.ConversationSubject(conversation),
			Op: assistantdomain.OpConversationCreated, Gen: 0, Writer: "node-a",
			Scope: assistantdomain.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "ada", ActorKind: iam.ActorHuman,
	})
	if err != nil {
		t.Fatal(err)
	}
	n.applyBody(assistantdomain.Domain{}.Stream(), 1, record, monday)
	if got := n.table("assistant_conversations"); !strings.Contains(got,
		"title_sealed=not an envelope, and nobody looks;") {
		t.Errorf("the applier did not write the sealed value through:\n%s", got)
	}
}

// otherDomains are the packages this domain may not reach at any depth: every
// other domain's state and writer, the engine that wires them, and every
// surface. A conversation is a copy of what one person was shown, and a
// domain that could read another domain's rows could put more in it.
var otherDomains = []string{
	"internal/iamdomain", "internal/worklet", "internal/doclet", "internal/artifactsdomain",
	"internal/artifactdata", "internal/search", "internal/usage", "internal/engine",
	"internal/estate", "internal/api", "internal/agent", "internal/tools",
	"internal/fleetsecrets",
}

// THE ASSISTANT DOMAIN READS NO OTHER DOMAIN, by its imports at any depth and by
// the tables its statements name. Mutation: import internal/iamdomain, or name
// an `iam_people` table in a query, and this names it.
func TestTheAssistantDomainReadsNoOtherDomain(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	const module = "github.com/crewlet/crewlet/"
	seen := map[string]bool{}
	queue := []string{"internal/assistantdomain"}
	var walked int
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if seen[pkg] {
			continue
		}
		seen[pkg] = true
		walked++
		for _, banned := range otherDomains {
			if pkg == banned || strings.HasPrefix(pkg, banned+"/") {
				t.Errorf("internal/assistantdomain reaches %s", pkg)
			}
		}
		info, err := build.ImportDir(filepath.Join(root, filepath.FromSlash(pkg)), 0)
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, imp := range info.Imports {
			if rel, ok := strings.CutPrefix(imp, module); ok {
				queue = append(queue, rel)
			}
		}
	}
	if walked < 5 {
		t.Fatalf("the walk read %d packages, so it is not reading the tree it guards", walked)
	}

	// THE STATEMENTS ARE THE STRING LITERALS, read off the syntax tree so a
	// comment's prose is never taken for a query.
	table := regexp.MustCompile(`\b(?:FROM|INTO|UPDATE|JOIN)\s+([a-z_][a-z0-9_]*)`)
	dir := filepath.Join(root, "internal", "assistantdomain")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var named int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, m := range table.FindAllStringSubmatch(lit.Value, -1) {
				named++
				if !strings.HasPrefix(m[1], "assistant_") {
					t.Errorf("%s names table %s, which is not this domain's", e.Name(), m[1])
				}
			}
			return true
		})
	}
	if named < 20 {
		t.Fatalf("the walk found %d table names, so it is not reading the statements", named)
	}
}

// A PROPOSAL'S QUERY ROUND-TRIPS SEALED: the request a proposal stores is the
// one it will send, its query included.
func TestAProposalsRequestReadsBackWhole(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	c := person(uuid.New(), "ada")
	turn := n.openTurn(c, n.conversation(c, ""))
	id := uuid.New()
	query := url.Values{"q": {"payroll review"}, "limit": {"5"}}
	if _, err := n.writer.CreateProposal(t.Context(), fresh(c), assistantdomain.NewProposal{
		Turn: turn, Proposal: id, Tool: "save_view", Scope: wire.ScopeServer, Method: "PUT",
		Path: "/worklet/views/1", Query: query, Body: []byte(`{"name":"Mine"}`),
		RequestDigest: hex64("b"), Digest: "sha256:" + hex64("c"), OpKey: op("approve"),
		Card: proposalCard(id), ExpiresAt: monday.Add(15 * 60e9),
	}); err != nil {
		t.Fatalf("create the proposal: %v", err)
	}
	got, err := n.reader.Proposal(t.Context(), c.Owner, id)
	if err != nil {
		t.Fatalf("read the proposal: %v", err)
	}
	if got.Query.Encode() != query.Encode() || string(got.Body) != `{"name":"Mine"}` ||
		got.Method != "PUT" || got.Path != "/worklet/views/1" || got.Card.Proposal == nil {
		t.Errorf("the proposal reads back as %+v", got)
	}
}
