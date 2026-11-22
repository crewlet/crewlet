package main

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// route is one of the run's routes on the bridge (the notes route, the skill
// route), which carries the run's token in its path: read from the
// environment, and never printed ([route.scrub]).
type route struct {
	url    string
	name   string
	client *http.Client
}

// newRoute is the route raw names, called name in every error; refused where
// raw is no http(s) address.
func newRoute(raw, name string) (*route, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The parse error quotes the route, so it is not wrapped.
		return nil, errors.New(name + " is not an http or https address")
	}
	return &route{
		url:  strings.TrimSuffix(raw, "/"),
		name: name,
		client: &http.Client{
			// A ROUTE NEVER REDIRECTS, and one followed would carry the
			// token's path to wherever it pointed: a 3xx is a failure.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// scrub is err, met doing op, with the route taken out: net/http's errors
// quote the whole address, token and all, and the run's error stream is no
// place for it.
func (r *route) scrub(op string, err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	return errors.New(op + " " + r.name + ": " + strings.ReplaceAll(err.Error(), r.url, r.name))
}
