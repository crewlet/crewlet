package jsapi

import (
	"errors"
	"fmt"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
)

// ClientError is err with the broker's answer a raw read decoded
// (*server.ApiError) handed over as the client decodes it too
// (*jetstream.APIError), so internal/jsprovision classifies it without
// linking nats-server ([jsprovision.Leaderless], [jsprovision.StreamAbsent]).
// err's words are kept; any other error comes back as it is.
func ClientError(err error) error {
	var raw *server.ApiError
	if !errors.As(err, &raw) {
		return err
	}
	return &clientError{err: err, api: &jetstream.APIError{Code: raw.Code,
		ErrorCode: jetstream.ErrorCode(raw.ErrCode), Description: raw.Description}}
}

// clientError is [ClientError]'s answer: err's words, both decodings beneath.
type clientError struct {
	err error
	api *jetstream.APIError
}

func (e *clientError) Error() string   { return fmt.Sprint(e.err) }
func (e *clientError) Unwrap() []error { return []error{e.err, e.api} }
