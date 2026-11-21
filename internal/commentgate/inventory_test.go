package commentgate

// excessInventory is, per package directory, the comment lines past
// blockBound the package held when the bound arrived. It only moves down.
var excessInventory = map[string]int{
	"internal/redact":           963,
	"internal/sandbox/executor": 35,
}

// rowInventory is the length, in characters, of each internal/README.md row
// that ran past rowBound when the bound arrived, by its first cell. It only
// moves down.
var rowInventory = map[string]int{
	"logging, secrets, provision, envref, envfile, redact, workkey, backoff, cron, period": 6125,
	"sandbox": 122900,
}
