package midtest

import "example.com/fixture/internal/waittest"

// Clock hands back waittest's, so a caller reaches Settle without naming
// waittest.
func Clock() waittest.Clock { return waittest.Clock{} }
