package expo

import (
	"context"
	"fmt"
	"strings"
)

// inspection is one read of the provisioner token and the account its robots
// are created on: the read both the Test button and the reconcile make.
type inspection struct {
	viewer  Actor
	account Account
	perms   []Permission
	apps    int
}

// admin reports a token whose actor manages the account's robots.
func (in inspection) admin() bool { return administers(in.perms) }

// stage is one read of an inspection.
type stage struct{ name string }

//nolint:gochecknoglobals // immutable descriptions, not state
var (
	stageViewer  = stage{name: "the token"}
	stageAccount = stage{name: "the account"}
	stageRole    = stage{name: "its role on the account"}
	stageApps    = stage{name: "the account's projects"}
)

// stageError is a read of an inspection that failed, naming which.
type stageError struct {
	stage stage
	err   error
}

func (e *stageError) Error() string { return fmt.Sprintf("read %s: %v", e.stage.name, e.err) }
func (e *stageError) Unwrap() error { return e.err }

// inspect reads, in order, who the token acts as, the account by name, what
// that actor holds on it and how many projects it has, stopping at the first
// read that fails. bound, where set, gives each call its own deadline.
func inspect(
	ctx context.Context, c *Client, account string,
	bound func(context.Context) (context.Context, context.CancelFunc),
) (inspection, error) {
	call := func(s stage, fn func(context.Context) error) error {
		callCtx, cancel := ctx, context.CancelFunc(func() {})
		if bound != nil {
			callCtx, cancel = bound(ctx)
		}
		defer cancel()
		if err := fn(callCtx); err != nil {
			return &stageError{stage: s, err: err}
		}
		return nil
	}
	var in inspection
	steps := []struct {
		stage stage
		read  func(context.Context) error
	}{
		{stageViewer, func(ctx context.Context) (err error) { in.viewer, err = c.Viewer(ctx); return err }},
		{stageAccount, func(ctx context.Context) (err error) { in.account, err = c.Account(ctx, account); return err }},
		{stageRole, func(ctx context.Context) (err error) { in.perms, err = c.ViewerPermissions(ctx, account); return err }},
		{stageApps, func(ctx context.Context) (err error) { in.apps, err = c.AppCount(ctx, account); return err }},
	}
	for _, s := range steps {
		if err := call(s.stage, s.read); err != nil {
			return in, err
		}
	}
	return in, nil
}

// actorWords names an actor for a sentence: "the robot ci-bot".
func actorWords(a Actor) string {
	kind := "user"
	if a.Kind == "Robot" {
		kind = "robot"
	}
	if name := strings.TrimSpace(a.Name); name != "" {
		return "the " + kind + " " + name
	}
	return "its " + kind
}

// permissionWords is a permission set for a sentence, or "no permission".
func permissionWords(perms []Permission) string {
	if len(perms) == 0 {
		return "no permission"
	}
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, string(p))
	}
	return strings.Join(out, ", ")
}
