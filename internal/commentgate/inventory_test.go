package commentgate

// excessInventory is, per package directory, the comment lines past
// blockBound the package held when the bound arrived. It only moves down.
var excessInventory = map[string]int{
	"internal/sandbox/executor": 34,
}

// rowInventory is the length, in characters, of each internal/README.md row
// that ran past rowBound when the bound arrived, by its first cell. It only
// moves down.
var rowInventory = map[string]int{}
