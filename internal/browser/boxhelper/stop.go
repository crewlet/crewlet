package boxhelper

import (
	"path"
	"time"

	"github.com/crewlet/crewlet/internal/shellword"
)

// StopTimeout bounds one run of stop: a supervisor that ignores its TERM is
// killed with its group after twice [stopGraceMS] and waited on for one more,
// with five seconds besides for node's start, the config's read and the lock.
const StopTimeout = 3*stopGraceMS*time.Millisecond + 5*time.Second

// StopScript is stop as a POSIX sh script for the box whose home is home: a
// box that holds no supervisor's record runs no supervisor, and answers at
// once; any other runs the helper's stop, every path quoted.
func StopScript(home string) string {
	folder := Folder(home)
	return "unset NODE_OPTIONS\n" +
		"[ -e " + shellword.Quote(path.Join(folder, "supervisor.json")) + " ] || exit 0\n" +
		"exec node " + shellword.Quote(path.Join(folder, "helper.mjs")) + " stop\n"
}

// StopCommand is the command the engine stops the browser a box runs with,
// in the box whose home is home: [StopScript] in a login shell, as ensure
// runs, so it finds the node that started the supervisor.
func StopCommand(home string) string {
	return "sh -lc " + shellword.Quote(StopScript(home))
}
