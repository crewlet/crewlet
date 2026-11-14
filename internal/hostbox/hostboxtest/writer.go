package hostboxtest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"syscall"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// Writer is a writer process's end of the matrix, made once in the
// re-executed test binary from what [KillAtEveryStep] handed it.
type Writer struct {
	// stop is the step to park at, from 1; 0 parks at none.
	stop int

	mu sync.Mutex
	// out announces on the matrix's pipe, a line a write.
	out *json.Encoder
	// ready is whether the setup is done: the steps before it are the
	// setup's, and are not the script's.
	ready bool
	// reached is how many of the script's steps the writer has reached.
	reached int
}

// NewWriter is this process's end of the matrix. A process [KillAtEveryStep]
// did not start is told so on stderr and exits 2: a writer announcing into a
// pipe nobody reads, or parking where nobody kills it, would only hang.
func NewWriter() *Writer {
	raw, ok := os.LookupEnv(stopEnv)
	stop, err := strconv.Atoi(raw)
	if !ok || err != nil || stop < 0 {
		refuse(fmt.Sprintf("%s=%q names no step, so KillAtEveryStep did not start this process", stopEnv, raw))
	}
	pipe := os.NewFile(announceFD, "hostboxtest-announcements")
	if _, err := pipe.Stat(); err != nil {
		refuse(fmt.Sprintf("file descriptor %d, the matrix's pipe, is not open: %v", announceFD, err))
	}
	// CLOSED ON EXEC, so no process the writer starts inherits it: one that
	// outlived the writer would hold the pipe open, and the matrix, reading it
	// to its end, would wait on that process rather than on the writer.
	syscall.CloseOnExec(int(pipe.Fd()))
	return &Writer{stop: stop, out: json.NewEncoder(pipe)}
}

// Steps is the observer the writer's writes are handed. It ignores every step
// before [Writer.Ready], which are the setup's, announces each one after, and
// at the step it was told to stop at parks until it is killed.
func (w *Writer) Steps() hostbox.CredentialSteps {
	return func(step hostbox.CredentialStep, name string) {
		w.mu.Lock()
		if !w.ready {
			w.mu.Unlock()
			return
		}
		w.reached++
		stopped := w.reached == w.stop
		w.announce(announcement{Kind: kindStep, Index: w.reached, Step: step, Name: name, Stopped: stopped})
		w.mu.Unlock()
		if stopped {
			park()
		}
	}
}

// Ready says the setup is done: the steps from here on are the script's.
func (w *Writer) Ready() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ready = true
	w.announce(announcement{Kind: kindReady})
}

// Done says the script reached its end without being stopped.
func (w *Writer) Done() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.announce(announcement{Kind: kindDone})
}

// announce writes one announcement, as one line in one write, which a pipe
// delivers whole. One the matrix can no longer read ends the writer, since
// nobody is there to stop or kill it.
func (w *Writer) announce(a announcement) {
	if err := w.out.Encode(a); err != nil {
		refuse(fmt.Sprintf("announcing %+v: %v", a, err))
	}
}

// park holds the writer at the step it was told to stop at until the matrix
// kills it: it reads its stdin, whose write end the matrix holds, and that
// ends only when the matrix closes it or is gone itself. Then the writer
// exits 3 rather than return into the write, so a parked writer never
// finishes a write behind a case's back.
func park() {
	_, _ = io.Copy(io.Discard, os.Stdin)
	fmt.Fprintln(os.Stderr, "hostboxtest: the matrix that parked this writer is gone, so it ends here "+
		"rather than finish the write")
	os.Exit(3)
}

// refuse ends a writer the matrix cannot drive, saying why.
func refuse(why string) {
	fmt.Fprintln(os.Stderr, "hostboxtest: "+why)
	os.Exit(2)
}
