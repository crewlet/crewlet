package cdp

// LogEnable is Log.enable: report what the browser itself logs about a page,
// the refusals of its content security policy among them. First consumer:
// the artifact render's console report (internal/artifactrender).
var LogEnable = Method[struct{}, struct{}]{name: "Log.enable"}

// LogEntryAdded is Log.entryAdded: one entry of the browser's own log. Its
// text can quote the page, so one past ReadLimit ends the session whose
// listener asked for it, and no other. First consumer: the console report.
var LogEntryAdded = Event[LogEntryAddedEvent]{name: "Log.entryAdded"}

// LogEntryAddedEvent is Log.entryAdded's parameters.
type LogEntryAddedEvent struct {
	Entry LogEntry `json:"entry"`
}

// LogEntry is Log.LogEntry.
type LogEntry struct {
	// Source is "javascript", "network", "security", "violation",
	// "intervention", "rendering" or another Chromium names.
	Source string `json:"source"`
	// Level is "verbose", "info", "warning" or "error".
	Level      string `json:"level"`
	Text       string `json:"text"`
	URL        string `json:"url"`
	LineNumber int64  `json:"lineNumber"`
}
