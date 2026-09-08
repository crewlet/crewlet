// Command gen prints the generated metrics reference.
//
// A command rather than a -update flag on the test, so regenerating is a
// gesture an operator or a contributor can run without knowing the test's
// name — and so the generation has exactly one implementation.
package main

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

func main() { fmt.Print(metrics.Reference()) }
