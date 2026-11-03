package gitref

// GitRefFormat is git's check_refname_format with no flags, read through the
// package's own refFormat: what git itself refuses, and nothing more. Without
// --allow-onelevel that includes a ref of one component — and "@" alone is
// one component, which is the whole of the refusal git spells separately for
// it (separately because it holds under --allow-onelevel too, a flag this
// never takes). The ref table holds it to git's own check-ref-format verdict
// on every row, because ValidRef's verdicts alone cannot show that refFormat
// reads git's rules: every rule ValidRef adds hides some of git's. Nothing in
// the engine asks git's unnarrowed rule, so it lives here, beside the one
// test that does.
func GitRefFormat(ref string) bool {
	components, ok := refFormat(ref, false)
	return ok && components >= 2
}
