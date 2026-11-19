package assistant_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
)

// A QUESTION'S IMAGES ARE HANDED TO ITS MODEL where its harness takes them:
// every round of the question carries the attached image's bytes and type,
// and the prompt says they are attached. Where the model takes no image,
// nothing is handed over, and the prompt says the model cannot see them and
// offers what works. Mutation: drop the images from the loop's request and
// the first case goes red.
func TestAQuestionsImagesAreHandedToItsModel(t *testing.T) {
	t.Parallel()
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{3}, 64)...)
	for name, c := range map[string]struct {
		reads bool
		says  string
	}{
		"a model that reads images": {true, "attached to this message"},
		"a model that reads none":   {false, "paste the text"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := evals.NewFiles(evals.NewClock().Now)
			f := assistanttest.NewRig(t, evals.RigOptions{Files: files, Images: c.reads,
				Tools:  []catalog.Tool{getTask(t)},
				Rounds: []evals.Round{evals.Say("The chart shows a rise.")}})
			if got := f.Svc.ReadsImages(); got != c.reads {
				t.Fatalf("ReadsImages = %v, want %v", got, c.reads)
			}
			shot, err := files.Attach(f.Person.ID, f.Conversation, "chart.png", png)
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := f.Svc.Start(context.Background(), assistant.TurnStart{Person: f.Person,
				Conversation: f.Conversation, Key: evals.Key("shot"), Input: "What does this show?",
				Read: f.Grant("shot"), Attachments: []uuid.UUID{shot.FileID}})
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			req := f.Model.Requests()[0]
			var prompt strings.Builder
			for _, m := range req.Messages {
				prompt.WriteString(m.Content)
			}
			if !strings.Contains(prompt.String(), "chart.png") || !strings.Contains(prompt.String(), c.says) {
				t.Fatalf("the prompt does not say %q of the image:\n%s", c.says, prompt.String())
			}
			switch {
			case c.reads && (len(req.Images) != 1 || req.Images[0].MediaType != "image/png" ||
				!bytes.Equal(req.Images[0].Data, png)):
				t.Errorf("the model was handed %d images, want the attached png", len(req.Images))
			case !c.reads && len(req.Images) != 0:
				t.Errorf("a model that reads no image was handed %d", len(req.Images))
			}
		})
	}
}

// A TASK TOO BIG FOR A TURN BECOMES A TASK FOR AN AGENT SEAT: where the person
// may file tasks, the system prompt tells the model it has no shell or files of
// its own and to propose a task for a seat that can do the work; where they
// may not, it says nothing of it. No tool hands a task to a run of its own.
// Mutation: drop the sentence and the first case goes red.
func TestABigTaskBecomesATaskForAnAgentSeat(t *testing.T) {
	t.Parallel()
	createTask := func(t *testing.T) catalog.Tool { return assistanttest.Tool(t, catalog.CreateTaskTool) }
	for name, c := range map[string]struct {
		tools func(t *testing.T) []catalog.Tool
		says  bool
	}{
		"may file tasks":     {func(t *testing.T) []catalog.Tool { return []catalog.Tool{getTask(t), createTask(t)} }, true},
		"may not file tasks": {func(t *testing.T) []catalog.Tool { return []catalog.Tool{getTask(t)} }, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := assistanttest.NewRig(t, evals.RigOptions{Tools: c.tools(t),
				Rounds: []evals.Round{evals.Say("Done.")}})
			accepted, err := f.Ask("big", "Redesign the brand assets from last month's work", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			system := f.Model.Requests()[0].Messages[0].Content
			if got := strings.Contains(system, "propose a task for one of the company's agent seats"); got != c.says {
				t.Errorf("the prompt speaks of a seat's task: %v, want %v", got, c.says)
			}
			for _, tool := range offered(t, f) {
				if strings.Contains(tool, "deep") {
					t.Errorf("the model was offered %s", tool)
				}
			}
		})
	}
}
