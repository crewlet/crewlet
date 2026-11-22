package engine

import (
	"os"
	"runtime/debug"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/nodebudget"
	"github.com/crewlet/crewlet/internal/nodememory"
	"github.com/crewlet/crewlet/internal/testbinary"
)

// THE GO RUNTIME'S MEMORY LIMIT is this process's share of the node's memory,
// never the node's whole (internal/nodebudget's package doc says why, and how
// Gitlet's reserve keeps the same lines). Set where the runtime is built
// ([Engine.startNative]), once a process, as only then is it known whether
// this node serves git and how its lanes were sized; an explicit GOMEMLIMIT,
// "off" included, wins. docs/guides/deployment.md, "The engine's memory
// limit", says what an operator sees.

// gitletMemory is what a node serving Gitlet holds outside this process: the
// memory its lanes were sized by, what its git transfers and background work
// may hold of it (buildGitletHost), and the lines it kept beside Gitlet.
type gitletMemory struct {
	memory nodememory.Memory
	git    int64
	lines  nodebudget.Lines
}

// budgetNode is what this node's memory lines are computed from: the
// executor's pools it runs, its store's connections and how its page cache is
// sized. Gitlet's reserve and the runtime's limit both read it.
func (e *Engine) budgetNode(boot *config.Bootstrap) nodebudget.Node {
	return nodebudget.Node{Work: gitWork(e.profile, boot),
		Conns: storeOptions(boot, nil, 0).NodeConnections(heldLogs(boot)), PageCacheSized: pageCacheSized()}
}

// explicitLimitSaid logs an explicit GOMEMLIMIT once a process.
var explicitLimitSaid sync.Once

// limitMemory sets the runtime's memory limit for the runtime being built: by
// what Gitlet holds and kept where it serves Gitlet, else by the node's memory
// read now less its page cache (nodebudget.Limit). An unreadable memory sets
// none and says so. In a test binary it applies a limit only where its case
// asks ([Options.setMemoryLimit], [SizedByTheMachine]): the suite runs many
// engines in one process, each sized by its case.
func (e *Engine) limitMemory(boot *config.Bootstrap, git *gitletMemory) {
	var m nodememory.Memory
	var held, outside int64
	if git != nil {
		m, held, outside = git.memory, git.git, git.git+git.lines.Outside()
	} else {
		var err error
		if m, err = nodememory.Read(); err != nil {
			log.Warn("engine_memory_limit_unset", "error", err)
			return
		}
		outside = e.budgetNode(boot).Of(m.Bytes, 0).Outside()
	}
	limit := nodebudget.Limit(m.Bytes, outside)
	if v, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		explicitLimitSaid.Do(func() {
			log.Info("engine_memory_limit_explicit", "GOMEMLIMIT", v, "would_set", limit,
				"memory", m.Bytes, "memory_from", m.From, "git_held", held, "outside", outside)
		})
		return
	}
	if limit == 0 {
		log.Warn("engine_memory_limit_unset", "error", "what the node holds outside the Go runtime is all of "+
			"its memory", "memory", m.Bytes, "memory_from", m.From, "git_held", held, "outside", outside)
		return
	}
	set := e.setMemoryLimit
	if set == nil {
		if testbinary.Running() && e.sizedByTheMachine == "" {
			return
		}
		set = debug.SetMemoryLimit
	}
	set(limit)
	log.Info("engine_memory_limit", "limit", limit, "memory", m.Bytes, "memory_from", m.From,
		"git_held", held, "outside", outside)
}
