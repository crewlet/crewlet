//go:build !(linux || darwin)

package speech

import "errors"

// loadRecognizer has no library to load off linux and darwin.
func loadRecognizer(string, Model, string) (recognizer, error) {
	return nil, errors.New("speech: this build carries no recognizer library")
}
