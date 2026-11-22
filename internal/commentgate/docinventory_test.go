package commentgate

// docInventory is, per package directory, the size in bytes of each package
// doc that ran past docBound when the bound arrived. It only moves down.
var docInventory = map[string]int{
	"internal/redact":           22827,
	"internal/sandbox/executor": 82611,
}
