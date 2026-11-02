package assistantdomain

// SealedColumns is [sealedColumns] as "table.column", for the test that holds
// it to the schema.
func SealedColumns() []string {
	out := make([]string, 0, len(sealedColumns))
	for _, c := range sealedColumns {
		out = append(out, c.table+"."+c.column)
	}
	return out
}
