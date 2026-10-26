package cards

import "github.com/crewlet/crewlet/internal/assistantdomain"

// The bounds a card and its summary are held to.
const (
	// MaxRows is the most rows one table card carries: a read answering more
	// is carded with its first MaxRows and a notice naming how many there
	// were, and the person opens the rest in the app.
	MaxRows = 1000

	// MaxBytes is one card's encoded size, the bound the assistant's log
	// stores a card under (assistantdomain.CardMaxBytes), so every card
	// built here is one the log accepts.
	MaxBytes = assistantdomain.CardMaxBytes

	// ModelBytes is the most of one step's result a model is handed:
	// sixteen kibibytes, a quarter of the 64 KiB a step reads
	// (exec.MaxResponseBytes), so a turn's forty steps at most
	// (internal/config's max_steps ceiling) fit a 200,000 token context
	// beside the prompt. What does not fit is dropped whole
	// (rows past the bound, an entity's list items) and the summary says
	// how much, rather than cut mid-value.
	ModelBytes = 16 << 10
)
