package engine

import (
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/notify"
)

// datadogPrompt is the alert prompt: a function, like the other third-party
// apps' helpers, since a lone variable would read as a prompt holding state.
func datadogPrompt() notify.Prompt { return datadog.Prompt{} }
