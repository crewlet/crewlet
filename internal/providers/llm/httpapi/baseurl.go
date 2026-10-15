package httpapi

import (
	"errors"
	"net/url"
	"strings"
)

// BaseURL is the endpoint a backend hands its SDK: raw TRIMMED, fallback where
// it is empty, and refused unless it is an http:// or https:// URL with a host.
//
// AT CONSTRUCTION, because both SDKs' WithBaseURL defer a parse failure to
// every request: a value that does not parse built a provider whose every
// call failed with "requestoption: WithBaseURL failed to parse url …", and
// net/url's error prints the WHOLE raw URL — userinfo included, so a gateway's
// password reached the classified error, the logs and the turn's events. The
// refusal here repeats no part of the value. Trimmed for the same reason the
// engine trims before judging it (config.ResolvedBaseURL): a value judged
// without its surrounding space and sent with it is a value nobody judged.
func BaseURL(raw, fallback string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback, nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrBaseURL
	}
	return s, nil
}

// ErrBaseURL is [BaseURL]'s refusal, which names nothing of the value.
var ErrBaseURL = errors.New("BaseURL is not an http:// or https:// URL with a host, " +
	"so no call could be sent on it")
