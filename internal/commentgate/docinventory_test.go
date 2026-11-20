package commentgate

// docInventory is, per package directory, the size in bytes of each package
// doc that ran past docBound when the bound arrived. It only moves down.
var docInventory = map[string]int{
	"internal/coord/kv":         31396,
	"internal/redact":           22955,
	"internal/sandbox/executor": 82836,
}
