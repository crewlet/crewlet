// Command alarmgen prints the generated alarm reference.
//
// A command rather than a -update flag on the test, so regenerating is a
// gesture a contributor can run without knowing the test's name — and so the
// generation has exactly one implementation.
package main

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

func main() { fmt.Print(statelog.AlarmReference()) }
