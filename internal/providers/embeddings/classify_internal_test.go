package embeddings

import (
	"context"
	"errors"
	"io"
	"net/url"
	"testing"
)

// A CANCELLATION IS THE CALLER'S, whatever shape it arrives in. The SDK hands
// back a bare context.Canceled today, but the transport wraps one in a
// *url.Error — which is also a net.Error — and a classifier that tested for a
// network failure first would call the caller stopping a transient provider
// fault. So the cancellation is tested for first, and this holds it there.
func TestACancellationIsNeverTheProvidersFault(t *testing.T) {
	t.Parallel()
	p := &Provider{model: "m"}
	for _, err := range []error{
		context.Canceled,
		&url.Error{Op: "Post", URL: "https://example.com/v1/embeddings", Err: context.Canceled},
	} {
		got := p.classify(err)
		if !errors.Is(got, context.Canceled) {
			t.Errorf("classify(%v) = %v, lost the cancellation", err, got)
		}
		for _, class := range []error{ErrRefused, ErrTransient, ErrConfiguration} {
			if errors.Is(got, class) {
				t.Errorf("classify(%v) = %v, classified as %v", err, got, class)
			}
		}
	}
	// And a transport failure that is not a cancellation is transient.
	broken := &url.Error{Op: "Post", URL: "https://example.com/v1/embeddings", Err: io.ErrUnexpectedEOF}
	if got := p.classify(broken); !errors.Is(got, ErrTransient) {
		t.Errorf("classify(%v) = %v, want ErrTransient", broken, got)
	}
}
