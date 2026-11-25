package mcpbridge

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// listMiddleware tells the session, once per opening, that the box's harness
// connected its bridge ([Session.Connected]): the first tools/list of the
// engine's own server it answered. The package doc's "A harness that never
// connected its bridge ran without it" says why the list and not the handshake.
// Told as the list is answered, a second list waiting on the first's hook,
// which only starts the record's write (the engine's collection waits for it);
// under the run's context, never the request's, as [binding.begin] tells its
// hook.
func (o *binding) listMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if method == "tools/list" && err == nil {
			o.listed.Do(func() {
				if hook := o.session.Connected; hook != nil {
					hook(o.ctx)
				}
			})
		}
		return res, err
	}
}
