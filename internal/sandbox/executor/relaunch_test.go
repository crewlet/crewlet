package executor_test

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// crossVMBox is a box whose home is mounted across a VM, as a container's is under
// Docker Desktop: a file the engine writes over one the box holds is missing to the
// job it starts next, unless the box itself removed or replaced it in between.
type crossVMBox struct {
	sandbox.Sandbox
	mu       sync.Mutex
	replaced map[string]os.FileInfo
	hidden   []string
}

func (b *crossVMBox) WriteFile(ctx context.Context, path string, content []byte) error {
	return b.write(path, func() error { return b.Sandbox.WriteFile(ctx, path, content) })
}

func (b *crossVMBox) WriteStream(ctx context.Context, path string, content io.Reader) error {
	return b.write(path, func() error { return b.Sandbox.WriteStream(ctx, path, content) })
}

// write notes path as replaced where a file was there before the write, read on
// the engine host, where a direct box's paths lie.
func (b *crossVMBox) write(path string, write func() error) error {
	_, err := os.Lstat(path)
	held := err == nil
	if err = write(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.replaced, path)
	if written, statErr := os.Lstat(path); held && statErr == nil {
		b.replaced[path] = written
	}
	return nil
}

// Exec runs cmd in the box, which sees anew every replaced file the command
// itself removed or replaced.
func (b *crossVMBox) Exec(ctx context.Context, cmd string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	res, err := b.Sandbox.Exec(ctx, cmd, opts)
	b.mu.Lock()
	defer b.mu.Unlock()
	for path, written := range b.replaced {
		if now, statErr := os.Lstat(path); statErr != nil || !os.SameFile(now, written) {
			delete(b.replaced, path)
		}
	}
	return res, err
}

// StartBackground starts the job with every file the engine replaced over one
// the box held out of its sight.
func (b *crossVMBox) StartBackground(ctx context.Context, cmd string, opts sandbox.ExecOptions) (string, error) {
	b.mu.Lock()
	for path := range b.replaced {
		if err := os.Rename(path, path+".unseen"); err != nil {
			b.mu.Unlock()
			return "", err
		}
		b.hidden = append(b.hidden, path)
	}
	clear(b.replaced)
	b.mu.Unlock()
	return b.Sandbox.StartBackground(ctx, cmd, opts)
}

// configuredHarness is [wiredHarness] that, as Claude Code does, ends at once on
// an MCP configuration it cannot read.
func configuredHarness(release string) string {
	return strings.Replace(wiredHarness(release), "prev=; for a; do",
		`prev=; for a; do if [ "$prev" = --mcp-config ] && ! [ -r "$a" ]; then `+
			`echo "MCP config file not found: $a" >&2; exit 1; fi; prev=$a; done`+"\nprev=; for a; do", 1)
}

// A RELAUNCH IN A REUSED BOX STARTS WITH ITS TOOL CONFIG, even in a box that does not
// see a file the engine replaced over one it held, since its start replaces none.
// Mutation: leave mcp.json out of the clear, and the continuing run never starts.
func TestARelaunchInAReusedBoxStartsWithItsToolConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		held bool
		want string
	}{
		{name: "its session held", held: true, want: "relaunch-token"},
		{name: "a session not in it", want: "fresh-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner, real := realBox(t)
			p := executor.PathsFor(real)
			writeBoxStandIn(t, real, p.BinDir()+"/claude", configuredHarness("2.1.310"))
			box := &crossVMBox{Sandbox: real, replaced: map[string]os.FileInfo{}}
			_, first := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin", MCPServers: bridgeAt("first-job")})
			if !first.Success {
				t.Fatalf("the first job failed: %s", boxFile(t, real, p.Err()))
			}

			resume := uuid.NewString()
			if tc.held {
				resume = first.SessionID
			}
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on where you stopped",
				FreshBrief: "the task, from the top", Resume: resume, MCPServers: bridgeAt("relaunch-token"),
				FreshWiring: &sandbox.Wiring{MCPServers: bridgeAt("fresh-token")}})
			if len(box.hidden) > 0 {
				t.Errorf("the relaunch's start wrote %v over files its box held", box.hidden)
			}
			if !res.Success {
				t.Fatalf("the relaunch's harness did not start: %s", boxFile(t, real, p.Err()))
			}
			if seen := boxFile(t, real, real.Home()+"/mcp-seen"); !strings.Contains(seen, tc.want) {
				t.Errorf("the harness started on %q, want its own wiring under %s", seen, tc.want)
			}
		})
	}
}
