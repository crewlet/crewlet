package usage

import "fmt"

// PagedStatement is one statement a paged read issues, with arguments.
type PagedStatement struct {
	Text string
	Args []any
}

// PagedStatements is every statement the paged reads issue — each formatted
// as a page that RESUMES inside its leading key formats it, after seat on
// node (person for the people's spend) on company day day, and for the page
// id page — keyed by what it reads. The resuming shape is the one whose plan
// matters: a keyset the planner had to read past rather than seek past is
// what a page re-read.
func PagedStatements(day, node, seat, person, page string) map[string]PagedStatement {
	past, pastArgs := keyset([]string{"node", "agent_id"}, Group{node, seat})
	byDay := func(statement, filter string, past string, pastArgs []any) PagedStatement {
		return PagedStatement{Text: fmt.Sprintf(statement, past),
			Args: append([]any{day, filter, filter}, pastArgs...)}
	}
	personPast, personArgs := keyset([]string{"node", "person"}, Group{node, person})
	readsPast, readsArgs := keyset([]string{"r.day", "r.node", "r.agent_id"}, Group{day, node, seat})
	vias := PageReadsQuery{Vias: []string{"search", "skill_loaded"}}
	statements := map[string]PagedStatement{
		"a day's spend cells":           byDay(spendDayStatement, "", past, pastArgs),
		"a day's spend cells of a seat": byDay(spendDayStatement, seat, past, pastArgs),
		"a day's names":                 byDay(dayNamesStatement, "", past, pastArgs),
		"a day's turn counts":           byDay(turnsDayStatement, "", past, pastArgs),
		"a day's turn counts of a seat": byDay(turnsDayStatement, seat, past, pastArgs),
		"a day's people's spend":        byDay(personDayStatement, "", personPast, personArgs),
		"a day's spend of a person":     byDay(personDayStatement, person, personPast, personArgs),
		"a day's seat days":             byDay(seatDayStatement, "", past, pastArgs),
		"a day's token totals":          byDay(seatDayTokensStatement, "", past, pastArgs),
		"a page's reads": {Text: fmt.Sprintf(pageReadsStatement, pageReadsWhere(PageReadsQuery{}, readsPast)),
			Args: append([]any{page, day, "2999-12-31"}, readsArgs...)},
		"a page's reads by some ways": {Text: fmt.Sprintf(pageReadsStatement, pageReadsWhere(vias, readsPast)),
			Args: append(append([]any{page, day, "2999-12-31"}, readsArgs...), "search", "skill_loaded")},
	}
	for _, table := range []string{tokensTable, turnsTable, personTokensTable} {
		for _, inclusive := range []bool{true, false} {
			statements[fmt.Sprintf("the next day of %s (inclusive %v)", table, inclusive)] = PagedStatement{
				Text: nextDayStatement(table, inclusive), Args: []any{day, "2999-12-31"}}
		}
	}
	return statements
}
