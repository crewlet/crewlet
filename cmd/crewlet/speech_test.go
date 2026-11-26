package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/speech"
)

// THE WORKER IS DISPATCHED UNDER THE NAME THE ENGINE STARTS IT BY: a rename on
// one side would leave every dictation starting a command the binary does not
// answer.
func TestTheSpeechWorkerIsDispatchedUnderItsOwnName(t *testing.T) {
	t.Parallel()
	if !slices.Contains(dispatchedCommands(t), speech.WorkerCommand) {
		t.Fatalf("runIn() dispatches no %q, the command the recognizer starts", speech.WorkerCommand)
	}
}

// `speech prepare` NEEDS ITS DIRECTORY, and says so before reading anything.
func TestSpeechPrepareNeedsADirectory(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := runSpeech([]string{"prepare"}, &stdout, &stderr); err == nil {
		t.Error("speech prepare ran with no -dir")
	}
	if err := runSpeech(nil, &stdout, &stderr); err == nil {
		t.Error("speech ran with no subcommand")
	}
}

// A NODE SWITCHED OFF HAS NO RECOGNIZER, and one on keeps a fetched model
// beside its database, on the data volume.
func TestTheRecognizerFollowsTheDictationBlock(t *testing.T) {
	t.Parallel()
	off := false
	boot := config.DefaultBootstrap()
	boot.Dictation.Enabled = &off
	if dictationRecognizer(&boot) != nil {
		t.Error("a node with dictation.enabled: false built a recognizer")
	}
	if got := speechDataDir(filepath.Join("/data", "crewlet.db")); got != "/data" {
		t.Errorf("a fetched model goes to %s, want the store's directory", got)
	}
	if got := speechDataDir(":memory:"); filepath.Base(got) != "crewlet-speech" {
		t.Errorf("an in-memory store keeps a fetched model in %s, want the native-library cache", got)
	}
}
