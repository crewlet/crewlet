package engine

import (
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/notify"
)

// datadogPrompt is the alert prompt, matching the other third-party apps' helpers.
//
// A function rather than a package-level value because every one of its
// neighbours is one, and a lone variable here would read as a prompt that
// holds state when it holds none.
func datadogPrompt() notify.Prompt { return datadog.Prompt{} }
