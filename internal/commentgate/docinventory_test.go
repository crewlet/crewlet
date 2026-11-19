package commentgate

// docInventory is, per package directory, the size in bytes of each package
// doc that ran past docBound when the bound arrived. It only moves down.
var docInventory = map[string]int{
	"internal/coord/kv":         31396,
	"internal/engine":           113165,
	"internal/redact":           23747,
	"internal/sandbox/executor": 83104,
}
