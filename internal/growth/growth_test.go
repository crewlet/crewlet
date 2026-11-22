package growth_test

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/period"
)

// Each class has its handling: the held ones are decided as a never-delegated
// act, and each asks the least level that may call it.
//
// Mutation: map public to operate's level, or let admin be asked at operate,
// and a seat asks for what its level does not allow.
func TestEveryClassHasItsHandling(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		class growth.Class
		held  bool
		act   delegated.Kind
		level growth.Level
	}{
		{growth.ClassRead, false, "", growth.LevelRead},
		{growth.ClassOperate, false, "", growth.LevelOperate},
		{growth.ClassPublic, true, delegated.KindPublicPost, growth.LevelOperate},
		{growth.ClassSpend, true, delegated.KindAdSpend, growth.LevelOperate},
		{growth.ClassAdmin, true, delegated.KindAccountAdmin, growth.LevelAdminister},
		{growth.ClassNever, false, "", ""},
	} {
		if c.class.Held() != c.held || c.class.Act() != c.act || c.class.Level() != c.level {
			t.Errorf("%s: held %v act %q level %q; want %v %q %q", c.class, c.class.Held(), c.class.Act(),
				c.class.Level(), c.held, c.act, c.level)
		}
		if c.held && c.act.Delegable() {
			t.Errorf("%s is held as %s, which a delegate may decide", c.class, c.act)
		}
	}
	if growth.ClassNever.Callable() || growth.Class("pay").Valid() {
		t.Error("never is callable, or an unknown class is valid")
	}
}

// A seat's level allows exactly the classes at or below it, an unset level
// reads read, and an unknown level allows nothing.
//
// Mutation: compare levels by name, or read an empty level as operate, and a
// seat written without a level asks for spend.
func TestALevelAllowsWhatItsRankAllows(t *testing.T) {
	t.Parallel()
	allows := map[growth.Level][]growth.Class{
		growth.LevelRead:       {growth.ClassRead},
		growth.LevelOperate:    {growth.ClassRead, growth.ClassOperate, growth.ClassPublic, growth.ClassSpend},
		growth.LevelAdminister: {growth.ClassRead, growth.ClassOperate, growth.ClassPublic, growth.ClassSpend, growth.ClassAdmin},
	}
	for level, want := range allows {
		for _, c := range growth.Classes() {
			got := level.Allows(c)
			if got != containsClass(want, c) {
				t.Errorf("%s allows %s: %v", level, c, got)
			}
		}
	}
	if growth.LevelOf("  ") != growth.LevelRead || growth.Level("owner").Allows(growth.ClassRead) {
		t.Error("an unset level is not read, or an unknown level allows a read")
	}
}

func containsClass(cs []growth.Class, c growth.Class) bool {
	for _, x := range cs {
		if x == c {
			return true
		}
	}
	return false
}

func budgetOp() growth.Operation {
	return growth.Operation{ID: "campaign.update", Method: "POST", Path: "/{campaign}", Resource: "Campaign",
		Class: growth.ClassOperate, Fields: []growth.FieldRule{
			{Field: "daily_budget", Class: growth.ClassSpend, Lower: growth.ClassOperate},
			{Field: "status", Class: growth.ClassSpend, Pausing: []string{"PAUSED"}},
			{Field: "audience", Class: growth.ClassSpend},
			{Field: "headline", Class: growth.ClassPublic},
		}}
}

// A generic update is classed by what it changes: a pause and a lower are
// operate, an enable and a raise spend, the highest change wins, and a class
// this build does not know is never.
//
// Mutation: class a write by its operation alone, read a lowered budget as a
// raise, or rank an unknown class, and a raise runs undecided, a brake waits
// on a person, or a typo lowers a raise to read.
func TestAnUpdateIsClassedByWhatItChanges(t *testing.T) {
	t.Parallel()
	op := budgetOp()
	for name, c := range map[string]struct {
		changes []growth.Change
		want    growth.Class
	}{
		"a pause":                {[]growth.Change{{Field: "status", Before: "ACTIVE", After: "PAUSED"}}, growth.ClassOperate},
		"an enable":              {[]growth.Change{{Field: "status", Before: "PAUSED", After: "ACTIVE"}}, growth.ClassSpend},
		"a lower":                {[]growth.Change{{Field: "daily_budget", Before: "5000", After: "4000"}}, growth.ClassOperate},
		"a raise":                {[]growth.Change{{Field: "daily_budget", Before: 4000.0, After: 5000.0}}, growth.ClassSpend},
		"an unreadable number":   {[]growth.Change{{Field: "daily_budget", Before: "a", After: "1"}}, growth.ClassSpend},
		"a new budget":           {[]growth.Change{{Field: "daily_budget", After: "1"}}, growth.ClassSpend},
		"a rename":               {[]growth.Change{{Field: "name", Before: "a", After: "b"}}, growth.ClassOperate},
		"new public text":        {[]growth.Change{{Field: "headline", After: "Hi"}}, growth.ClassPublic},
		"a pause and new text":   {[]growth.Change{{Field: "status", After: "PAUSED"}, {Field: "headline", After: "x"}}, growth.ClassPublic},
		"text and a raise":       {[]growth.Change{{Field: "headline", After: "x"}, {Field: "daily_budget", Before: "1", After: "2"}}, growth.ClassSpend},
		"a lower and a widening": {[]growth.Change{{Field: "daily_budget", Before: "2", After: "1"}, {Field: "audience", After: "all"}}, growth.ClassSpend},
		"nothing named":          {nil, growth.ClassOperate},
	} {
		if got := growth.Classify(op, c.changes); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	unknown := budgetOp()
	unknown.Fields[0].Lower = "operat"
	if got := growth.Classify(unknown, []growth.Change{{Field: "daily_budget", Before: "2", After: "1"}}); got != growth.ClassNever {
		t.Errorf("a lower to a class this build does not know is %s, want never", got)
	}
}

func validCatalog() growth.Catalog {
	return growth.Catalog{
		Kind: "test_ads", API: "Test Ads API", Version: "v1",
		Roles:       []growth.Role{{ID: "ADMIN", Name: "Admin", Note: "Everything."}, {ID: "READ", Name: "Read", Note: "Reads."}},
		Permissions: []growth.Permission{{ID: "ads_read", Note: "Reads ads."}},
		Operations: []growth.Operation{
			{ID: "campaigns.list", Method: "GET", Path: "/campaigns", Class: growth.ClassRead, Permissions: []string{"ads_read"}},
			budgetOp(),
			{ID: "users.create", Method: "POST", Path: "/users", Class: growth.ClassAdmin, Roles: []string{"ADMIN"}},
			{ID: "billing.charge", Method: "POST", Path: "/charge", Class: growth.ClassNever,
				Reason: "The vendor's terms reserve payments to its own console."},
		},
		Tools: []growth.Tool{
			{Name: "test_ads_read", Description: "A tool.", Parameters: map[string]any{"type": "object"}, Class: growth.ClassRead, Operations: []string{"campaigns.list"}},
			{Name: "test_ads_pause", Description: "A tool.", Parameters: map[string]any{"type": "object"}, Class: growth.ClassOperate, Operations: []string{"campaign.update"}},
			{Name: "test_ads_raise", Description: "A tool.", Parameters: map[string]any{"type": "object"}, Class: growth.ClassSpend, Operations: []string{"campaign.update"}},
			{Name: "test_ads_admin", Description: "A tool.", Parameters: map[string]any{"type": "object"}, Class: growth.ClassAdmin, Operations: []string{"users.create"}},
		},
	}
}

// A catalog is held to its rules: every row classed, a reason on every
// never, reads read, writes not read, known roles and permissions, and tools
// whose class their operations can come to.
//
// Mutation: drop any one check, and its broken catalog registers.
func TestACatalogIsHeldToItsRules(t *testing.T) {
	t.Parallel()
	if err := validCatalog().Validate(); err != nil {
		t.Fatalf("a valid catalog was refused: %v", err)
	}
	query := validCatalog()
	query.Operations = append(query.Operations, growth.Operation{ID: "reports.run", Method: "POST",
		Path: "/reports:run", Class: growth.ClassRead, Reads: true})
	if err := query.Validate(); err != nil {
		t.Errorf("a POST that only reads was refused as read: %v", err)
	}
	// A WRITE A PRODUCT CLASSES PER CALL names the classes below its own it can
	// come to, and a tool of one of them may list it.
	mutate := validCatalog()
	mutate.Operations = append(mutate.Operations, growth.Operation{ID: "batch.mutate", Method: "POST",
		Path: "/batch:mutate", Class: growth.ClassAdmin, Classes: []growth.Class{growth.ClassOperate, growth.ClassSpend}})
	mutate.Tools[1].Operations = append(mutate.Tools[1].Operations, "batch.mutate")
	if err := mutate.Validate(); err != nil {
		t.Errorf("an operate tool listing a write that can come to operate was refused: %v", err)
	}
	for name, breakIt := range map[string]func(*growth.Catalog){
		"no kind":              func(c *growth.Catalog) { c.Kind = "" },
		"a never without why":  func(c *growth.Catalog) { c.Operations[3].Reason = "" },
		"a write classed read": func(c *growth.Catalog) { c.Operations[2].Class = growth.ClassRead },
		"a query classed spend": func(c *growth.Catalog) {
			c.Operations[2].Reads, c.Operations[2].Class = true, growth.ClassSpend
		},
		"a read classed spend": func(c *growth.Catalog) { c.Operations[0].Class = growth.ClassSpend },
		"an unknown role":      func(c *growth.Catalog) { c.Operations[2].Roles = []string{"OWNER"} },
		"an unknown scope":     func(c *growth.Catalog) { c.Operations[0].Permissions = []string{"ads_write"} },
		"a duplicate op":       func(c *growth.Catalog) { c.Operations[1].ID = "campaigns.list" },
		"an unknown method":    func(c *growth.Catalog) { c.Operations[1].Method = "PURGE" },
		"a tool's missing op":  func(c *growth.Catalog) { c.Tools[0].Operations = []string{"nope"} },
		"a tool off its op":    func(c *growth.Catalog) { c.Tools[3].Class = growth.ClassSpend },
		"a never tool":         func(c *growth.Catalog) { c.Tools[0].Class = growth.ClassNever },
		"a tool not described": func(c *growth.Catalog) { c.Tools[1].Description = "" },
		"a tool off its kind":  func(c *growth.Catalog) { c.Tools[1].Name = "other_ads_pause" },
		"a tool of no name":    func(c *growth.Catalog) { c.Tools[1].Name = "test_ads" },
		"a role without note":  func(c *growth.Catalog) { c.Roles[0].Note = "" },
		"a rule lowering up":   func(c *growth.Catalog) { c.Operations[1].Fields[0].Lower = growth.ClassAdmin },
		"an unknown lower":     func(c *growth.Catalog) { c.Operations[1].Fields[0].Lower = "operat" },
		"a read tool's write": func(c *growth.Catalog) {
			c.Operations[1].States = []growth.StateRule{{When: "archived", Class: growth.ClassRead}}
			c.Tools[0].Operations = []string{"campaign.update"}
		},
		"a state with no name": func(c *growth.Catalog) {
			c.Operations[2].States = []growth.StateRule{{Class: growth.ClassPublic}}
		},
		"a further class at its own": func(c *growth.Catalog) {
			c.Operations[2].Classes = []growth.Class{growth.ClassAdmin}
		},
		"a further class above it": func(c *growth.Catalog) {
			c.Operations[0].Classes = []growth.Class{growth.ClassOperate}
		},
		"a further class never": func(c *growth.Catalog) {
			c.Operations[2].Classes = []growth.Class{growth.ClassNever}
		},
	} {
		c := validCatalog()
		c.Operations = append([]growth.Operation(nil), c.Operations...)
		c.Operations[1].Fields = append([]growth.FieldRule(nil), c.Operations[1].Fields...)
		c.Tools = append([]growth.Tool(nil), c.Tools...)
		c.Roles = append([]growth.Role(nil), c.Roles...)
		breakIt(&c)
		if c.Validate() == nil {
			t.Errorf("%s: the catalog validated", name)
		}
	}
}

// A TOOL NAME IS ONE PRODUCT'S: a catalog listing a tool another already
// lists is refused at registration, though each name starts with its own
// kind's, since one kind may begin another's.
//
// Mutation: drop RegisterFunc's walk over the other catalogs, and the second
// registers.
func TestACatalogListingAnothersToolIsRefused(t *testing.T) {
	t.Parallel()
	object := map[string]any{"type": "object"}
	catalog := func(kind string) growth.Catalog {
		return growth.Catalog{Kind: kind, API: "Shared API", Version: "v1",
			Operations: []growth.Operation{{ID: "list", Method: "GET", Path: "/list", Class: growth.ClassRead}},
			Tools: []growth.Tool{{Name: "shared_ads_list", Class: growth.ClassRead, Operations: []string{"list"},
				Description: "List.", Parameters: object}}}
	}
	growth.Register(catalog("shared"))
	defer func() {
		if recover() == nil {
			t.Error("a second catalog listing shared_ads_list registered")
		}
		if _, ok := growth.CatalogOf("shared_ads"); ok {
			t.Error("the refused catalog is registered")
		}
	}()
	growth.Register(catalog("shared_ads"))
}

// An operation naming roles is permitted to an identity holding one of them,
// and one naming none to every identity.
func TestARoleIsPermittedWhatItsOperationNames(t *testing.T) {
	t.Parallel()
	c := validCatalog()
	users, _ := c.Operation("users.create")
	list, _ := c.Operation("campaigns.list")
	if growth.Permits(users, []string{"READ"}) || !growth.Permits(users, []string{"READ", "ADMIN"}) ||
		!growth.Permits(list, nil) {
		t.Error("a role is permitted what its operation does not name, or refused what it does")
	}
}

// Money round-trips exactly through micros for currencies with 0, 2, 3 and 6
// minor digits, a vendor's minor units round down, and nothing passes
// through a float.
//
// Mutation: convert through float64, or round up, and some amount comes back
// a micro off, or more is sent than was allowed.
func TestMoneyNeverPassesThroughAFloat(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		for _, digits := range []int{0, 2, 3, 6} {
			minor := r.Int64N(1_000_000_000_000)
			micros, err := growth.FromMinor(minor, digits)
			if err != nil {
				t.Fatalf("%d minor units at %d digits: %v", minor, digits, err)
			}
			if back, _ := growth.ToMinor(micros, digits); back != minor {
				t.Fatalf("%d minor units at %d digits came back %d", minor, digits, back)
			}
			text, _ := growth.FormatAmount(micros, digits)
			parsed, err := growth.ParseAmount(text)
			if err != nil || parsed != micros {
				t.Fatalf("%d micros printed %q, read back %d (%v)", micros, text, parsed, err)
			}
		}
	}
	if m, _ := growth.ParseAmount("0.123456"); m != 123456 {
		t.Errorf("0.123456 read %d micros", m)
	}
	if got, _ := growth.ToMinor(1_999_999, 2); got != 199 {
		t.Errorf("1.999999 is %d cents, want 199: a vendor is never sent more than allowed", got)
	}
	if got := growth.RoundDown(10_015_000, 10_000); got != 10_010_000 {
		t.Errorf("rounded down to %d", got)
	}
	for _, bad := range []string{"", "-1", "1.1234567", "1e3", "1,00", "1234567890123"} {
		if _, err := growth.ParseAmount(bad); err == nil {
			t.Errorf("%q parsed as an amount", bad)
		}
	}
	if _, err := growth.FormatAmount(1, 7); err == nil {
		t.Error("seven minor digits formatted")
	}
}

// A named range is cut on the account's own calendar: two accounts asked at
// one instant can be on different days, and a day across a clock change is
// still one day.
//
// Mutation: cut the range in UTC or the host's zone, and Tokyo's yesterday is
// Los Angeles' day before.
func TestARangeIsCutOnTheAccountsOwnCalendar(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 3, 30, 0, 0, time.UTC)
	tokyo, _ := period.LoadZone("Asia/Tokyo")
	la, _ := period.LoadZone("America/Los_Angeles")
	for _, c := range []struct {
		zone       *time.Location
		r          growth.Range
		start, end string
	}{
		{tokyo, growth.RangeYesterday, "2026-10-31", "2026-10-31"},
		{la, growth.RangeYesterday, "2026-10-30", "2026-10-30"},
		{la, growth.RangeLast7Days, "2026-10-24", "2026-10-30"},
		{tokyo, growth.RangeMonthToDate, "2026-11-01", "2026-11-01"},
		{la, growth.RangeLast28Days, "2026-10-03", "2026-10-30"},
	} {
		d, err := c.r.Resolve(now, c.zone)
		if err != nil || d.Start != c.start || d.End != c.end {
			t.Errorf("%s in %s: %+v (%v), want %s to %s", c.r, c.zone, d, err, c.start, c.end)
		}
	}
	if _, err := growth.RangeYesterday.Resolve(now, nil); err == nil {
		t.Error("a range was cut with no zone")
	}
	if growth.RowLimit(0) != growth.ReportDefaultRows || growth.RowLimit(5000) != growth.ReportMaxRows {
		t.Error("a report's rows are not held to their bounds")
	}
}

// Every metric declares a scale and an aggregation, a weighted one weighs by
// a metric that sums, and no publisher figure shares an advertiser's name.
//
// Mutation: weigh search_position by itself, or rename ad_earnings to
// ad_spend, and a board sums the wrong thing.
func TestEveryMetricSaysHowItIsCountedAndCombined(t *testing.T) {
	t.Parallel()
	seen := map[growth.Metric]bool{}
	for _, s := range growth.Metrics() {
		if seen[s.Metric] || s.Scale == "" || s.Aggregate == "" {
			t.Errorf("%s: listed twice, or no scale or aggregation", s.Metric)
		}
		seen[s.Metric] = true
		if s.Aggregate == growth.AggregateWeighted {
			w, ok := s.WeightedBy.Spec()
			if !ok || w.Aggregate != growth.AggregateSum {
				t.Errorf("%s weighs by %q, which is no summed metric", s.Metric, s.WeightedBy)
			}
		}
	}
	for _, publisher := range []growth.Metric{growth.AdEarnings, growth.AppAdEarnings, growth.AdImpressionsServed} {
		if strings.HasPrefix(string(publisher), "ad_spend") || publisher == growth.AdImpressions {
			t.Errorf("%s shares an advertiser's name", publisher)
		}
	}
	if !growth.AdSpend.Money() || growth.AdClicks.Money() || growth.Metric("ctr").Valid() {
		t.Error("money is misread, or a ratio is a metric")
	}
}

// A role offers the classes of the operations it may call, an operation that
// names no role counting for every role, and the levels those classes need.
//
// Mutation: count only operations that name the role, and a read-only role
// reads as offering nothing.
func TestARoleOffersTheClassesOfWhatItMayCall(t *testing.T) {
	t.Parallel()
	offers := validCatalog().RoleOffers()
	if len(offers) != 2 {
		t.Fatalf("got %d offers", len(offers))
	}
	admin, read := offers[0], offers[1]
	if !slices.Contains(admin.Classes, growth.ClassAdmin) || !slices.Contains(admin.Levels, growth.LevelAdminister) {
		t.Errorf("Admin offers %v at %v", admin.Classes, admin.Levels)
	}
	if slices.Contains(read.Classes, growth.ClassAdmin) || !slices.Contains(read.Classes, growth.ClassRead) ||
		!slices.Contains(read.Classes, growth.ClassSpend) {
		t.Errorf("Read offers %v", read.Classes)
	}
}

// A CATALOG REGISTERED AS A FUNCTION IS BUILT AT FIRST USE, once: a large
// catalog costs nothing at init, beside a catalog whose kind neither begins
// its own nor is begun by it, which can share none of its tool names.
//
// Mutation: build at registration, or rebuild on each read, and the counter
// reads one before any read, or two after the second.
func TestALazyCatalogIsBuiltOnceAtFirstUseAndValidated(t *testing.T) {
	renamed := func(kind string) growth.Catalog {
		c := validCatalog()
		c.Kind = kind
		for i := range c.Tools {
			c.Tools[i].Name = kind + strings.TrimPrefix(c.Tools[i].Name, "test_ads")
		}
		return c
	}
	growth.Register(renamed("lazy_beside"))
	builds := 0
	growth.RegisterFunc("lazy_ads", func() growth.Catalog {
		builds++
		return renamed("lazy_ads")
	})
	if builds != 0 {
		t.Fatalf("the catalog was built %d times at registration", builds)
	}
	if _, ok := growth.CatalogOf("lazy_ads"); !ok || builds != 1 {
		t.Fatalf("the first read built it %d times (found %v)", builds, ok)
	}
	if growth.CatalogOf("lazy_ads"); builds != 1 {
		t.Errorf("a second read built it again: %d", builds)
	}
}
