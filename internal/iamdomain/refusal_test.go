package iamdomain_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A RECORD'S REFUSAL SAYS WHETHER A PERMISSION WOULD LIFT IT.
//
// A surface answers the two kinds differently: one a permission lifts names
// it, so the caller knows what to ask an administrator for, and one the
// record's own rule makes names none, because no permission would change it.
// Both were one sentinel wrapped in a sentence, so the identity directory's
// surface answered every refusal as the first kind and named every permission
// there is — an administrator minting a person's token was told to ask an
// administrator for access.
//
// Walked through the real writer, applier and reader, one row per kind of
// refusal: a party without admin enrolling, sweeping and minting a service
// account's token names admin; a permission the minting party cannot confer
// names that permission; a person's token minted by anybody else, a permission
// the owner does not hold, an owner who may not act, the row that binds a
// Tier A token, a name this build does not know and an invitation for a seat
// the chart retired name none; and a gesture through a machine token names
// none and says a person must be present — the mint's arm included, which
// used to answer as an ordinary refusal and so as a missing permission — and
// a sign-in ceremony or a mint through the Dashboard Assistant names none and
// says its person takes it themselves.
//
// Mutations: give the person arm of the mint grants [admin] and its row goes
// red; build a refusal with fmt.Errorf again and errors.As finds nothing; drop
// the machine-token sentinel from the mint's token arm and that row goes red.
func TestEveryRecordRefusalSaysWhetherAPermissionLiftsIt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	ctx := rig.t.Context()
	owner := tokenOwner(t, rig, "jane.doe")
	suspended := tokenOwner(t, rig, "sam.away")
	if err := rig.draining(func() error {
		_, err := rig.writer.SetStage(ctx, suspended, iam.StageSuspended,
			operationKey(), "on leave")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rig.drain()
	service := uuid.Must(uuid.NewV7()).String()
	binding := uuid.Must(uuid.NewV7()).String()
	for _, in := range []iamdomain.Creation{
		{PersonID: service, Kind: iam.KindMachine, Stage: iam.StageActive,
			Name: "Release pipeline", Login: "svc:release",
			Grants: []iam.Grant{iam.GrantWorklet}, OpID: operationKey(),
			Reason: "a pipeline"},
		{PersonID: binding, Kind: iam.KindMachine, Stage: iam.StageActive,
			Login: iam.TokenLogin("ops"), OpID: operationKey(),
			Reason: "binds ops"},
	} {
		if err := rig.enrol(in); err != nil {
			t.Fatal(err)
		}
	}
	rig.drain()

	user := rig.writer.As(principalNamed("ana.user", iam.KindPerson, iam.Apps))
	throughToken := iam.Principal{
		ID: uuid.MustParse(owner), Kind: iam.KindPerson, Login: "jane.doe",
		Stage: iam.StageActive, Grants: iam.AllGrants,
		Via: iam.MachineTokenName(uuid.Must(uuid.NewV7()).String()),
	}
	tokenAdmin := *anAdmin(rig)
	tokenAdmin.OperatorID = iam.MachineTokenName(uuid.Must(uuid.NewV7()).String())
	assisted := func(p iam.Principal) *iamdomain.Writer {
		p.Via = iam.AssistName(uuid.Must(uuid.NewV7()))
		p.AssistScope = iam.AssistScopeExec
		return rig.writer.As(p)
	}
	assistAdmin := assisted(principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin}))
	assistOwner := assisted(iam.Principal{
		ID: uuid.MustParse(owner), Kind: iam.KindPerson, Login: "jane.doe",
		Stage: iam.StageActive, Grants: iam.AllGrants,
	})
	mint := func(w *iamdomain.Writer, in iamdomain.TokenMint) error {
		_, _, err := mintFor(t, rig, w, in)
		return err
	}
	for _, tc := range []struct {
		name    string
		gesture func() error
		lifts   []iam.Grant // the permissions that lift it; empty for none
		token   bool        // asked through a machine token
		assist  bool        // asked through the Dashboard Assistant
	}{
		{"an enrolment by a party without admin", func() error {
			_, err := user.Create(ctx, iamdomain.Creation{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
				Stage: iam.StageActive, Login: "svc:widen",
				Grants: []iam.Grant{iam.GrantWorklet}, OpID: operationKey(),
				Reason: "a pipeline",
			})
			return err
		}, []iam.Grant{iam.GrantAdmin}, false, false},
		{"a sweep by a party without admin", func() error {
			_, err := user.Sweep(ctx, defaultHorizons)
			return err
		}, []iam.Grant{iam.GrantAdmin}, false, false},
		{"a service account's token minted without admin", func() error {
			return mint(user, iamdomain.TokenMint{PersonID: service,
				Grants: []iam.Grant{iam.GrantWorklet}})
		}, []iam.Grant{iam.GrantAdmin}, false, false},
		{"a permission the minting party cannot confer", func() error {
			return mint(asOwner(rig, owner, iam.GrantWorklet), iamdomain.TokenMint{
				PersonID: owner, Grants: []iam.Grant{iam.GrantDoclet}})
		}, []iam.Grant{iam.GrantDoclet}, false, false},
		{"a person's token minted by an administrator", func() error {
			return mint(anAdmin(rig), iamdomain.TokenMint{PersonID: owner})
		}, []iam.Grant{}, false, false},
		{"a permission the owner does not hold", func() error {
			return mint(asOwner(rig, owner), iamdomain.TokenMint{PersonID: owner,
				Grants: []iam.Grant{iam.GrantAdmin}})
		}, []iam.Grant{}, false, false},
		{"an owner who may not act", func() error {
			return mint(asOwner(rig, suspended), iamdomain.TokenMint{
				PersonID: suspended})
		}, []iam.Grant{}, false, false},
		{"the row that binds a Tier A token", func() error {
			return mint(anAdmin(rig), iamdomain.TokenMint{PersonID: binding})
		}, []iam.Grant{}, false, false},
		{"a name this build does not know", func() error {
			_, err := anAdmin(rig).Create(ctx, iamdomain.Creation{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
				Stage: iam.StageActive, Login: "svc:future",
				Grants: []iam.Grant{"swarm"}, OpID: operationKey(),
				Reason: "a pipeline",
			})
			return err
		}, []iam.Grant{}, false, false},
		{"an invitation for a seat the chart no longer offers", func() error {
			_, err := nodeWriter(rig).Redeem(ctx, iamdomain.Redemption{
				PersonID: uuid.Must(uuid.NewV7()).String(), Stage: iam.StageActive,
				Email: "x@example.com", Login: "x.joiner", Password: aPassword(),
				Invitation: uuid.Must(uuid.NewV7()).String(), InvitationSecret: "s",
				Seat: "nowhere", OpID: operationKey(), Reason: "redeemed",
			})
			return err
		}, []iam.Grant{}, false, false},
		{"an administrative record through a machine token", func() error {
			_, err := tokenAdmin.InvalidateAll(ctx, operationKey(), "a restore")
			return err
		}, []iam.Grant{}, true, false},
		{"a token minted through a machine token", func() error {
			return mint(rig.writer.As(throughToken),
				iamdomain.TokenMint{PersonID: owner})
		}, []iam.Grant{}, true, false},
		{"an enrolment through the Dashboard Assistant", func() error {
			_, err := assistAdmin.Create(ctx, iamdomain.Creation{
				PersonID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
				Stage: iam.StageActive, Login: "svc:assisted",
				Grants: []iam.Grant{iam.GrantWorklet}, OpID: operationKey(),
				Reason: "a pipeline",
			})
			return err
		}, []iam.Grant{}, false, true},
		{"a token minted through the Dashboard Assistant", func() error {
			return mint(assistOwner, iamdomain.TokenMint{PersonID: owner})
		}, []iam.Grant{}, false, true},
	} {
		err := tc.gesture()
		var refusal *iamdomain.Refusal
		if !errors.As(err, &refusal) {
			t.Errorf("%s: answered %v, want a refusal", tc.name, err)
			continue
		}
		if !errors.Is(err, iamdomain.ErrRefused) {
			t.Errorf("%s: the refusal is not ErrRefused (%v)", tc.name, err)
		}
		if refusal.Grants == nil || !slices.Equal(refusal.Grants, tc.lifts) {
			t.Errorf("%s: the refusal names %v as what lifts it, want %v (%v)",
				tc.name, refusal.Grants, tc.lifts, err)
		}
		if got := errors.Is(err, iamdomain.ErrTokenRefused); got != tc.token {
			t.Errorf("%s: ErrTokenRefused is %t, want %t (%v)", tc.name, got,
				tc.token, err)
		}
		if got := errors.Is(err, iamdomain.ErrAssistRefused); got != tc.assist {
			t.Errorf("%s: ErrAssistRefused is %t, want %t (%v)", tc.name, got,
				tc.assist, err)
		}
	}
}

// NOTHING IN THIS PACKAGE BUILDS A REFUSAL BUT [iamdomain.Refusal]'s TWO
// CONSTRUCTORS.
//
// A refusal built as `fmt.Errorf("%w: …", ErrRefused)` still compares as one,
// so nothing would notice that it no longer says whether a permission lifts
// it — the surface would answer it as an unclassified fault. So the walk holds
// the package's source to three rules: [iamdomain.ErrRefused] is named only at
// its declaration, inside the refusal's own methods and as the target of an
// errors.Is read; [iamdomain.ErrTokenRefused] and [iamdomain.ErrAssistRefused]
// only at their declarations, as a read's target, and as an argument of the
// constructor that names no permission, since neither a token's refusal nor
// the assistant's is ever lifted by one; and a refusal's
// literal only inside the two constructors. It fails a walk that read fewer
// files than the package holds today, since a walk of the wrong directory
// finds nothing to object to. Mutation: put `fmt.Errorf("%w: …", ErrRefused)`
// back at any refusal site and it goes red.
func TestErrRefusedIsOnlyEverBuiltAsARefusal(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		for _, decl := range file.Decls {
			for _, offence := range refusalOffences(decl) {
				t.Errorf("%s: %s", fset.Position(offence.Pos()), offenceText(offence))
			}
		}
	}
	if read < 20 {
		t.Fatalf("the walk read %d source files, want the package's own (20 or "+
			"more) — it is reading the wrong directory", read)
	}
}

// refusalOffences is every node in decl that builds a refusal outside the
// rules [TestErrRefusedIsOnlyEverBuiltAsARefusal] states.
func refusalOffences(decl ast.Decl) []ast.Node {
	var (
		function, receiver string
		allowed            = map[*ast.Ident]bool{}
		offences           []ast.Node
	)
	if fn, ok := decl.(*ast.FuncDecl); ok {
		function = fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) == 1 {
			typ := fn.Recv.List[0].Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			if id, ok := typ.(*ast.Ident); ok {
				receiver = id.Name
			}
		}
	}
	// FIRST, every identifier a rule admits where it stands: a declared
	// name, a read's target, and an argument of the constructor a token's
	// refusal is built through.
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			for _, id := range n.Names {
				allowed[id] = true
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Is" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "errors" &&
					len(n.Args) == 2 {
					if id, ok := n.Args[1].(*ast.Ident); ok {
						allowed[id] = true
					}
				}
			}
			if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "forbidden" {
				for _, arg := range n.Args {
					if id, ok := arg.(*ast.Ident); ok && (id.Name == "ErrTokenRefused" ||
						id.Name == "ErrAssistRefused") {
						allowed[id] = true
					}
				}
			}
		}
		return true
	})
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			switch n.Name {
			case "ErrRefused":
				if !allowed[n] && receiver != "Refusal" {
					offences = append(offences, n)
				}
			case "ErrTokenRefused", "ErrAssistRefused":
				if !allowed[n] {
					offences = append(offences, n)
				}
			}
		case *ast.CompositeLit:
			if id, ok := n.Type.(*ast.Ident); ok && id.Name == "Refusal" &&
				function != "needs" && function != "forbidden" {
				offences = append(offences, n)
			}
		}
		return true
	})
	return offences
}

// offenceText says what an offence is.
func offenceText(n ast.Node) string {
	if id, ok := n.(*ast.Ident); ok {
		return id.Name + " is named outside a refusal's constructors — build " +
			"the refusal with needs or forbidden, which say whether a " +
			"permission lifts it"
	}
	return "a Refusal literal outside needs and forbidden"
}
