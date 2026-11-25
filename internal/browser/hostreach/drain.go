package hostreach

import (
	"context"
	"fmt"
	"net"
)

// drain reads every connection the port's listeners queued before the
// check's command ended. On each listener in turn the engine connects last,
// with its own token, and waits for that token to be read: a listener's queue
// is first in, first out, so every connection queued before it has been
// accepted by then, however slowly the engine got to it. It stops at once on
// a port somebody else reached, which withholds the browser either way.
//
// Its error is ctx's own where the check's time ran out first, else the
// engine's own connection's.
func (p *port) drain(ctx context.Context, dial func(ctx context.Context, network, address string) (net.Conn, error)) error {
	for at, l := range p.listeners {
		if p.disturbed() {
			return nil
		}
		addr := l.Addr()
		conn, err := dial(ctx, addr.Network(), addr.String())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("connecting to %s: %w", addr, err)
		}
		_, err = conn.Write([]byte(p.tokens.drain + "\n"))
		if err == nil {
			select {
			case <-p.drained[at]:
			case <-p.strangers:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		_ = conn.Close()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("writing to %s: %w", addr, err)
		}
	}
	return nil
}
