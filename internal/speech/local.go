package speech

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LocalConfig configures [Local].
type LocalConfig struct {
	// Dir is `dictation.recognizer_dir`: a directory `crewlet speech prepare`
	// wrote (the library for this platform and the model, each in a
	// directory of its name), read and never written. Empty looks beside the
	// binary ([BesideBinary]), then fetches: the library into the
	// native-library cache, the model under DataDir.
	Dir string

	// DataDir is where a fetched model is kept, under speech/<name>.
	DataDir string

	// Capacity caps live streams; 0 is [DefaultCapacity].
	Capacity int
}

// DefaultCapacity is four live streams per available core: a stream measured
// 0.19 of a core, so four leave a quarter of every core to the engine.
func DefaultCapacity() int { return 4 * runtime.GOMAXPROCS(0) }

// UnloadAfter is how long a worker stays loaded with no live stream: long
// enough to keep a working session's dictations warm (a load is about two
// seconds), short enough to hand back the model's ~300 MB soon after.
const UnloadAfter = 10 * time.Minute

// RetryAfter is how long a failed preparation is answered before the next
// dictation tries again: a host with no route to the model does not start a
// download per microphone press.
const RetryAfter = time.Minute

// readyTimeout bounds a worker's load, measured at two seconds: a minute is
// far past a loaded host and still bounded.
const readyTimeout = time.Minute

// stopGrace is how long a worker told to exit is waited for before it is
// killed: it exits as soon as it reads the end of its input.
const stopGrace = 5 * time.Second

// preparingEvery is how often a waiting stream hears preparation progress.
const preparingEvery = 250 * time.Millisecond

// BesideBinary is where a container image or a package bakes the recognizer
// (as `crewlet speech prepare` lays it out): <prefix>/share/crewlet/speech for
// a binary in <prefix>/bin.
const BesideBinary = "share/crewlet/speech"

// Local is the local recognizer: one worker process (see the package doc)
// serving every stream on this node, started on the first dictation.
type Local struct {
	cfg      LocalConfig
	model    Model
	capacity int
	now      func() time.Time

	// native is this platform's library, and supported whether there is one.
	native    nativePin
	supported bool

	// Test seams, nil in the engine: library readies the library, locate
	// the model directory, and command starts a worker.
	library func(ctx context.Context, progress func(float64)) (string, error)
	locate  func(ctx context.Context, progress func(float64)) (string, error)
	command func(library, modelDir string) (*exec.Cmd, error)

	unloadAfter time.Duration
	stopping    chan struct{}
	stopOnce    sync.Once

	mu       sync.Mutex
	live     int
	worker   *worker
	starting *starting
	failure  *UnavailableError
	failedAt time.Time
	idle     *time.Timer
	nextID   uint32
}

var _ Streamer = (*Local)(nil)

// NewLocal builds the local recognizer. Nothing loads until [Local.Open].
func NewLocal(cfg LocalConfig) *Local {
	capacity := cfg.Capacity
	if capacity <= 0 {
		capacity = DefaultCapacity()
	}
	native, supported := nativeFor(thisPlatform)
	return &Local{cfg: cfg, model: DefaultModel, capacity: capacity, now: time.Now,
		native: native, supported: supported, unloadAfter: UnloadAfter, stopping: make(chan struct{})}
}

// Capacity is how many streams may be live at once.
func (l *Local) Capacity() int { return l.capacity }

// Availability implements [Streamer].
func (l *Local) Availability() Availability {
	if l.library == nil && !l.supported {
		return Availability{Reason: ReasonUnsupportedPlatform}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failure != nil && l.now().Sub(l.failedAt) < RetryAfter {
		return Availability{Reason: l.failure.Reason}
	}
	return Availability{Available: true}
}

// Open implements [Streamer].
func (l *Local) Open(ctx context.Context, opts Options) (Stream, error) {
	if err := checkOptions(opts); err != nil {
		return nil, err
	}
	if l.library == nil && !l.supported {
		return nil, &UnavailableError{Reason: ReasonUnsupportedPlatform}
	}
	l.mu.Lock()
	select {
	case <-l.stopping:
		l.mu.Unlock()
		return nil, &UnavailableError{Reason: ReasonFailed, Err: errors.New("the engine is stopping")}
	default:
	}
	if l.live >= l.capacity {
		l.mu.Unlock()
		return nil, ErrBusy
	}
	if l.worker == nil && l.starting == nil && l.failure != nil && l.now().Sub(l.failedAt) < RetryAfter {
		err := l.failure
		l.mu.Unlock()
		return nil, err
	}
	l.live++
	if l.idle != nil {
		l.idle.Stop()
		l.idle = nil
	}
	w, st := l.worker, l.starting
	if w == nil && st == nil {
		st = l.startLocked() //nolint:contextcheck // the recognizer's own lifetime (startLocked)
	}
	l.mu.Unlock()
	if w == nil {
		var err error
		if w, err = st.wait(ctx, opts.Preparing); err != nil {
			l.release()
			return nil, err
		}
	}
	s, err := w.open(l.id())
	if err != nil {
		l.release()
		return nil, err
	}
	return s, nil
}

func (l *Local) id() uint32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	return l.nextID
}

// release gives back one stream's capacity, arming the idle unload when it
// was the last.
func (l *Local) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live--
	if l.live == 0 && l.worker != nil && l.idle == nil {
		l.idle = time.AfterFunc(l.unloadAfter, l.unloadIfIdle)
	}
}

func (l *Local) unloadIfIdle() {
	l.mu.Lock()
	w := l.worker
	if l.live != 0 || w == nil {
		l.mu.Unlock()
		return
	}
	l.worker, l.idle = nil, nil
	l.mu.Unlock()
	log.Info("speech_worker_unloaded", "pid", w.pid(), "idle", l.unloadAfter.String())
	w.stop()
}

// Close stops the worker and refuses every later Open.
func (l *Local) Close() {
	l.stopOnce.Do(func() { close(l.stopping) })
	l.mu.Lock()
	w := l.worker
	l.worker = nil
	if l.idle != nil {
		l.idle.Stop()
		l.idle = nil
	}
	l.mu.Unlock()
	if w != nil {
		w.stop()
		<-w.exited
	}
}

// starting is one preparation every concurrent Open waits on.
type starting struct {
	done     chan struct{}
	progress atomic.Uint64
	worker   *worker
	err      error
}

func (st *starting) set(p float64) { st.progress.Store(math.Float64bits(p)) }

func (st *starting) get() float64 { return math.Float64frombits(st.progress.Load()) }

// wait waits for the preparation, telling preparing its progress whenever
// it moved.
func (st *starting) wait(ctx context.Context, preparing func(float64)) (*worker, error) {
	if preparing == nil {
		preparing = func(float64) {}
	}
	tick := time.NewTicker(preparingEvery)
	defer tick.Stop()
	told := st.get()
	preparing(told)
	for {
		select {
		case <-st.done:
			return st.worker, st.err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
			if p := st.get(); p != told {
				told = p
				preparing(p)
			}
		}
	}
}

// startLocked starts a preparation on a goroutine of the recognizer's own,
// so a dictation that gives up does not stop a download the next one needs.
func (l *Local) startLocked() *starting {
	st := &starting{done: make(chan struct{})}
	l.starting = st
	go l.prepare(st) //nolint:contextcheck // the recognizer's own lifetime, not the first dictation's (see above)
	return st
}

func (l *Local) prepare(st *starting) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-l.stopping:
			cancel()
		case <-ctx.Done():
		}
	}()
	began := l.now()
	w, err := l.startWorker(ctx, st)
	l.mu.Lock()
	l.starting = nil
	var unavailable *UnavailableError
	if err != nil {
		if !errors.As(err, &unavailable) {
			unavailable = &UnavailableError{Reason: ReasonFailed, Err: err}
		}
		l.failure, l.failedAt = unavailable, l.now()
	} else {
		l.worker, l.failure = w, nil
		if l.live == 0 {
			l.idle = time.AfterFunc(l.unloadAfter, l.unloadIfIdle)
		}
	}
	l.mu.Unlock()
	if err != nil {
		log.Error("speech_prepare_failed", "reason", string(unavailable.Reason), "error", err.Error(),
			"hint", "dictation is offered again after "+RetryAfter.String())
		st.err = unavailable
	} else {
		log.Info("speech_worker_started", "pid", w.pid(), "model", l.model.Name,
			"took", l.now().Sub(began).Round(time.Millisecond).String())
		st.worker = w
	}
	close(st.done)
}

// startWorker readies the library and the model, then starts a worker and
// waits for it to load. Preparation's progress is by bytes: the library's
// archive, then the model's download, then the load.
func (l *Local) startWorker(ctx context.Context, st *starting) (*worker, error) {
	share := 0.0
	if total := l.native.archive.Size + l.model.Download(); total > 0 {
		share = float64(l.native.archive.Size) / float64(total)
	}
	library, err := l.libraryPath(ctx, func(p float64) { st.set(0.95 * share * p) })
	if err != nil {
		return nil, &UnavailableError{Reason: ReasonModelUnavailable, Err: err}
	}
	dir, err := l.modelDir(ctx, func(p float64) { st.set(0.95 * (share + (1-share)*p)) })
	if err != nil {
		return nil, &UnavailableError{Reason: ReasonModelUnavailable, Err: err}
	}
	st.set(0.95)
	w, err := l.spawn(ctx, library, dir)
	if err != nil {
		return nil, &UnavailableError{Reason: ReasonFailed, Err: err}
	}
	select {
	case <-l.stopping:
		w.stop()
		return nil, &UnavailableError{Reason: ReasonFailed, Err: errors.New("the engine is stopping")}
	default:
	}
	st.set(1)
	return w, nil
}

// libraryPath is the verified library's C API: the operator's prepared
// directory's, else the binary's, else the native-library cache's, fetched
// there when absent.
func (l *Local) libraryPath(ctx context.Context, progress func(float64)) (string, error) {
	if l.library != nil {
		return l.library(ctx, progress)
	}
	p := l.native
	dir, ok, err := l.prepared(p.Name(), p.Verify)
	switch {
	case err != nil:
		return "", err
	case ok:
		return p.Library(dir), nil
	}
	dir = p.cacheDir()
	if p.Verify(dir) == nil {
		return p.Library(dir), nil
	}
	log.Info("speech_library_fetching", "release", SherpaVersion, "dir", dir, "bytes", p.archive.Size)
	if err := p.Fetch(ctx, dir, func(done int64) { progress(float64(done) / float64(p.archive.Size)) }); err != nil {
		return "", err
	}
	log.Info("speech_library_fetched", "release", SherpaVersion, "dir", dir)
	return p.Library(dir), nil
}

// modelDir is the verified directory of the model: the operator's, else the
// binary's, else the data directory's, fetched there when absent.
func (l *Local) modelDir(ctx context.Context, progress func(float64)) (string, error) {
	if l.locate != nil {
		return l.locate(ctx, progress)
	}
	m := l.model
	dir, ok, err := l.prepared(m.Name, m.Verify)
	switch {
	case err != nil:
		return "", err
	case ok:
		return dir, nil
	}
	dir = filepath.Join(l.cfg.DataDir, "speech", m.Name)
	if m.Verify(dir) == nil {
		return dir, nil
	}
	log.Info("speech_model_fetching", "model", m.Name, "dir", dir, "bytes", m.Download())
	if err := m.Fetch(ctx, dir, func(done, total int64) { progress(float64(done) / float64(total)) }); err != nil {
		return "", err
	}
	log.Info("speech_model_fetched", "model", m.Name, "dir", dir)
	return dir, nil
}

// prepared finds name in a prepared directory: `dictation.recognizer_dir`,
// where a copy that does not verify is an error rather than a download (its
// operator wants no egress), else beside the binary.
func (l *Local) prepared(name string, verify func(dir string) error) (string, bool, error) {
	if l.cfg.Dir != "" {
		dir := filepath.Join(l.cfg.Dir, name)
		if err := verify(dir); err != nil {
			return "", false, fmt.Errorf("dictation.recognizer_dir holds no verified %s (%w). Run "+
				"`crewlet speech prepare -dir %s` with this release, or unset "+
				"dictation.recognizer_dir to download it", name, err, l.cfg.Dir)
		}
		return dir, true, nil
	}
	if root, err := besideBinary(); err == nil {
		if dir := filepath.Join(root, name); verify(dir) == nil {
			return dir, true, nil
		}
	}
	return "", false, nil
}

// besideBinary is [BesideBinary] for the running binary.
func besideBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(filepath.Dir(exe)), filepath.FromSlash(BesideBinary)), nil
}

// spawn starts a worker and waits for its first frame: ready, or the cause it
// could not load.
func (l *Local) spawn(ctx context.Context, library, dir string) (*worker, error) {
	cmd, err := l.workerCommand(library, dir)
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &stderrLog{}
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the speech worker: %w", err)
	}
	w := &worker{l: l, cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout),
		streams: map[uint32]*localStream{}, exited: make(chan struct{})}
	first := make(chan error, 1)
	go func() {
		f, readErr := readFrame(w.out)
		switch {
		case readErr != nil:
			first <- fmt.Errorf("the speech worker exited before it loaded: %w", readErr)
		case f.kind == frameFailed:
			first <- fmt.Errorf("the speech worker could not load: %s", f.payload)
		case f.kind != frameReady:
			first <- fmt.Errorf("the speech worker sent frame %d before it was ready", f.kind)
		default:
			first <- nil
		}
	}()
	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()
	select {
	case err = <-first:
	case <-timer.C:
		err = fmt.Errorf("the speech worker did not load within %s", readyTimeout)
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	go w.read()
	return w, nil
}

func (l *Local) workerCommand(library, dir string) (*exec.Cmd, error) {
	if l.command != nil {
		return l.command(library, dir)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("the speech worker is this binary, and os.Executable could not name it: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, fmt.Errorf("the speech worker is this binary, and its path could not be resolved: %w", err)
	}
	// NO CONTEXT: the worker outlives the dictation that starts it, and ends
	// when its input is closed ([worker.stop]).
	return exec.Command(exe, WorkerCommand, "-library", library, "-model", l.model.Name, //nolint:gosec,noctx // this binary, with paths it prepared
		"-model-dir", dir), nil
}

// workerExited is told by a worker's reader that its process ended.
func (l *Local) workerExited(w *worker, status error, streams int) {
	l.mu.Lock()
	if l.worker == w {
		l.worker = nil
		if l.idle != nil {
			l.idle.Stop()
			l.idle = nil
		}
	}
	l.mu.Unlock()
	if w.wasStopped() {
		return
	}
	// NEVER SILENT: a worker that ends on its own crashed, and the
	// dictations it served were told so.
	statusText := "exit 0"
	if status != nil {
		statusText = status.Error()
	}
	log.Error("speech_worker_exited", "pid", w.pid(), "status", statusText, "streams", streams,
		"hint", "the next dictation starts a new worker")
}

// worker is one running `speech-worker` process.
type worker struct {
	l      *Local
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	exited chan struct{}

	writeMu sync.Mutex

	mu      sync.Mutex
	streams map[uint32]*localStream
	gone    bool
	stopped bool
}

func (w *worker) pid() int { return w.cmd.Process.Pid }

func (w *worker) wasStopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped
}

func (w *worker) open(id uint32) (*localStream, error) {
	s := &localStream{w: w, id: id, results: make(chan Result, workerQueue), gone: make(chan struct{})}
	w.mu.Lock()
	if w.gone {
		w.mu.Unlock()
		return nil, ErrLost
	}
	w.streams[id] = s
	w.mu.Unlock()
	if err := w.send(frame{kind: frameOpen, id: id}); err != nil {
		w.mu.Lock()
		delete(w.streams, id)
		w.mu.Unlock()
		return nil, err
	}
	return s, nil
}

func (w *worker) send(f frame) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := writeFrame(w.stdin, f); err != nil {
		return fmt.Errorf("%w: %w", ErrLost, err)
	}
	return nil
}

// read routes the worker's frames to their streams until it exits, then
// fails every stream still open.
func (w *worker) read() {
	for {
		f, err := readFrame(w.out)
		if err != nil {
			break
		}
		w.mu.Lock()
		s := w.streams[f.id]
		if f.kind == frameDone {
			delete(w.streams, f.id)
		}
		w.mu.Unlock()
		if s == nil {
			continue
		}
		switch f.kind {
		case framePartial:
			s.deliver(Result{Text: string(f.payload)})
		case frameFinal:
			s.deliver(Result{Final: true, Text: string(f.payload)})
		case frameDone:
			s.end(nil)
		}
	}
	status := w.cmd.Wait()
	w.mu.Lock()
	w.gone = true
	open := w.streams
	w.streams = nil
	w.mu.Unlock()
	// The recognizer lets go of the worker BEFORE its streams learn it died:
	// a dictation that sees ErrLost and opens again starts a new worker,
	// never this dead one.
	w.l.workerExited(w, status, len(open))
	for _, s := range open {
		s.end(ErrLost)
	}
	close(w.exited)
}

// stop ends the worker's input, which it exits on, and kills it after
// [stopGrace] if it has not.
func (w *worker) stop() {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
	w.writeMu.Lock()
	_ = w.stdin.Close()
	w.writeMu.Unlock()
	go func() {
		timer := time.NewTimer(stopGrace)
		defer timer.Stop()
		select {
		case <-w.exited:
		case <-timer.C:
			_ = w.cmd.Process.Kill()
		}
	}()
}

// localStream is one stream on a worker. Its results are sent and closed by
// the worker's reader alone.
type localStream struct {
	w       *worker
	id      uint32
	results chan Result
	gone    chan struct{}

	mu       sync.Mutex
	finished bool
	closed   bool
	ended    bool
	released bool
	err      error
}

func (s *localStream) Results() <-chan Result { return s.results }

func (s *localStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *localStream) Send(pcm []byte) error {
	if err := checkPCM(pcm); err != nil {
		return err
	}
	s.mu.Lock()
	switch {
	case s.closed || s.finished:
		s.mu.Unlock()
		return ErrClosed
	case s.ended:
		s.mu.Unlock()
		return ErrLost
	}
	s.mu.Unlock()
	for len(pcm) > 0 {
		n := min(len(pcm), maxFramePayload)
		if err := s.w.send(frame{kind: frameAudio, id: s.id, payload: pcm[:n]}); err != nil {
			return err
		}
		pcm = pcm[n:]
	}
	return nil
}

func (s *localStream) Finish() error {
	s.mu.Lock()
	if s.closed || s.finished {
		s.mu.Unlock()
		return ErrClosed
	}
	s.finished = true
	s.mu.Unlock()
	return s.w.send(frame{kind: frameFinish, id: s.id})
}

func (s *localStream) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	close(s.gone)
	_ = s.w.send(frame{kind: frameClose, id: s.id})
	s.release()
}

// deliver hands r on unless the stream was closed.
func (s *localStream) deliver(r Result) {
	select {
	case s.results <- r:
	case <-s.gone:
	}
}

// end closes the results once, recording why.
func (s *localStream) end(err error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	if !s.closed {
		s.err = err
	}
	s.mu.Unlock()
	close(s.results)
	s.release()
}

func (s *localStream) release() {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	s.mu.Unlock()
	s.w.l.release()
}

// stderrLog logs a worker's stderr a line at a time. The worker writes only
// load failures there (never audio or text); a line is bounded.
type stderrLog struct {
	mu      sync.Mutex
	partial []byte
}

// stderrLine bounds one logged line of worker stderr.
const stderrLine = 512

func (s *stderrLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(s.partial[:i]))
		s.partial = s.partial[i+1:]
		if line != "" {
			log.Warn("speech_worker_stderr", "line", line[:min(len(line), stderrLine)])
		}
	}
	if len(s.partial) > stderrLine {
		s.partial = s.partial[:0]
	}
	return len(p), nil
}
