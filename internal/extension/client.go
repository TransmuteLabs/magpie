package extension

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

// Event is a worker's acknowledged output. Its consumer must finish writing it
// before returning; that return releases the worker's single stream credit.
type Event struct {
	Name string
	Data json.RawMessage
}

type OutcomeError struct {
	Outcome string
	Cause   error
}

func (e *OutcomeError) Error() string {
	return "extension operation " + e.Outcome
}
func (e *OutcomeError) Unwrap() error { return e.Cause }

type pendingCall struct {
	ctx          context.Context
	scope        sdk.Scope
	frames       chan sdk.Frame
	seq          uint64
	credit       bool
	ended        bool
	terminal     bool
	hostID       string
	processHosts map[int]string
	hostWG       sync.WaitGroup
	canceled     bool
	cancelAt     time.Time
}

type workerWrite struct {
	frame        sdk.Frame
	encoded      []byte
	done         chan error
	probe        chan *workerWriteReservation
	release      <-chan struct{}
	ctx          context.Context
	writeClaimed *atomic.Bool
}

type workerWriteReservation struct {
	mu        sync.Mutex
	retired   bool
	committed chan struct{}
	work      workerWrite
}

func (reservation *workerWriteReservation) commit(work workerWrite, begin func() error) (bool, error) {
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.retired {
		return false, nil
	}
	if err := begin(); err != nil {
		return false, err
	}
	reservation.retired = true
	reservation.work = work
	close(reservation.committed)
	return true, nil
}

func (reservation *workerWriteReservation) retire() workerWrite {
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	reservation.retired = true
	return reservation.work
}

type workerClient struct {
	group         *componentGroup
	mu            sync.Mutex
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	cancel        context.CancelFunc
	done          chan struct{}
	readDone      chan struct{}
	writeDone     chan struct{}
	logDone       chan struct{}
	pending       map[string]*pendingCall
	next          atomic.Uint64
	slots         chan struct{}
	regularSlots  chan struct{}
	responseSlots chan struct{}
	controlSlots  chan struct{}
	nativeSlots   chan struct{}
	writes        chan workerWrite
	cancelFrames  chan sdk.Frame
	lifetime      context.Context
	hostWG        sync.WaitGroup
	cancelWG      sync.WaitGroup
	err           error
	hostCall      func(context.Context, sdk.Scope, string, json.RawMessage) (any, error)
	hostCounts    [sdk.ProcessLaneCount + 1]int
	processExit   chan struct{}
	earlyError    bool
}

type workerAdmission func(context.Context) (finish func(confirmed bool), err error)

type workerCallPreparation struct {
	auth  func(context.Context) (json.RawMessage, error)
	admit func(context.Context, func() error) error
	begin func(context.Context) error
}

// The child gets ordinary process essentials and its own state/home. In
// particular, environment-based provider credentials are not inherited.
func workerEnv(home string) []string {
	var out []string
	for _, name := range []string{
		"PATH", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT",
		"TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "TZ",
	} {
		if value, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return append(out, "HOME="+home, "USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"APPDATA="+filepath.Join(home, "appdata"))
}

func startWorker(ctx context.Context, root string, entry Installed, bun string,
	hostCall func(context.Context, sdk.Scope, string, json.RawMessage) (any, error),
) (*workerClient, error) {
	directory := filepath.Join(root, entry.Directory)
	program := filepath.Join(directory, filepath.FromSlash(entry.Manifest.Entry))
	args := append([]string(nil), entry.Manifest.Args...)
	if entry.Manifest.Runtime == "bun" {
		if bun == "" {
			var err error
			bun, err = resolveBun(ctx)
			if err != nil {
				return nil, err
			}
		}
		args = append([]string{program}, args...)
		program = bun
	}
	// The handshake deadline does not own a serving module's lifetime.
	lifetime, cancel := context.WithCancel(context.Background())
	var transport *moduleTransport
	var err error
	if entry.Manifest.Runtime == "native" {
		transport, err = openInstalledNativeTransport(ctx, root, entry)
	} else {
		var home string
		home, err = workerHome(root, entry.Manifest.ID)
		if err == nil {
			transport, err = openProcessTransport(lifetime, directory, program, args, home)
		}
	}
	if err != nil {
		cancel()
		return nil, err
	}
	stop := func() {
		cancel()
		if transport.stop != nil {
			transport.stop()
		}
	}
	client := &workerClient{
		stdin: transport.input, stdout: transport.output, cancel: stop,
		done: make(chan struct{}), readDone: make(chan struct{}),
		writeDone: make(chan struct{}),
		logDone:   make(chan struct{}), pending: make(map[string]*pendingCall),
		slots: make(chan struct{}, sdk.MaxConcurrent), writes: make(chan workerWrite),
		regularSlots:  make(chan struct{}, sdk.MaxConcurrent/2-2),
		responseSlots: make(chan struct{}, sdk.MaxConcurrent/2),
		controlSlots:  make(chan struct{}, 2),
		nativeSlots:   make(chan struct{}, sdk.MaxConcurrent/2),
		lifetime:      lifetime, hostCall: hostCall,
		cancelFrames: make(chan sdk.Frame, sdk.MaxConcurrent),
		earlyError:   entry.Manifest.Runtime == "bun" && !entry.Manifest.Contract.Has(sdk.ProviderSavedSignIn),
	}
	if entry.Manifest.Runtime != "native" {
		client.processExit = make(chan struct{})
	}
	go func() {
		defer close(client.logDone)
		// Drain even an overlong line, without retaining or logging request
		// data a module may have mistakenly printed to stderr.
		_, _ = io.Copy(io.Discard, transport.diagnostics)
	}()
	go client.writeLoop()
	go client.read()
	go func() {
		// StdoutPipe must be drained before Wait closes it. On a protocol
		// failure read cancels the process and releases both pipes.
		<-client.readDone
		err := transport.wait()
		if client.processExit != nil {
			close(client.processExit)
		}
		<-client.logDone
		client.fail(err)
		<-client.writeDone
		client.hostWG.Wait()
		client.cancelWG.Wait()
		close(client.done)
	}()
	var actual sdk.Manifest
	result, err := client.call(ctx, "sdk.describe", sdk.Scope{Plugin: entry.Manifest.ID}, nil, nil, nil)
	if err == nil {
		err = json.Unmarshal(result, &actual)
	}
	if err == nil {
		actual.Runtime, actual.Entry, actual.Args = entry.Manifest.Runtime, entry.Manifest.Entry, entry.Manifest.Args
		err = actual.Validate()
	}
	if err == nil {
		declared := publicManifestBytes(actual)
		installed := publicManifestBytes(entry.Manifest)
		if string(declared) != string(installed) {
			err = errors.New("extension handshake does not match its installed contract")
		}
	}
	if err != nil {
		client.close()
		return nil, err
	}
	return client, nil
}

func publicManifestBytes(manifest sdk.Manifest) []byte {
	public := manifest.Public()
	slices.Sort(public.Contract.Capabilities)
	data, _ := json.Marshal(public)
	return data
}

func workerHome(root, id string) (string, error) {
	base, err := NewCatalog(root).open(false)
	if err != nil {
		return "", err
	}
	defer base.Close()
	state, err := catalogPrivateDir(base, "runtime")
	if err != nil {
		return "", err
	}
	defer state.Close()
	module, err := catalogPrivateDir(state, id)
	if err != nil {
		return "", err
	}
	defer module.Close()
	home, err := catalogPrivateDir(module, "home")
	if err != nil {
		return "", err
	}
	defer home.Close()
	return filepath.Abs(home.Name())
}

func (c *workerClient) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.cancel()
	_ = c.stdin.Close()
	_ = c.stdout.Close()
}

func (c *workerClient) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err == nil
}

func (c *workerClient) write(frame sdk.Frame) error {
	ctx, cancel := context.WithTimeout(c.lifetime, 15*time.Second)
	defer cancel()
	return c.send(ctx, frame)
}

func (c *workerClient) send(ctx context.Context, frame sdk.Frame) error {
	work := newWorkerWrite(ctx, frame)
	select {
	case c.writes <- work:
	case <-ctx.Done():
		return &OutcomeError{Outcome: "not-started", Cause: ctx.Err()}
	case <-c.lifetime.Done():
		return &OutcomeError{Outcome: "not-started", Cause: errors.New("extension worker stopped")}
	}
	return c.awaitWrite(ctx, work)
}

func newWorkerWrite(ctx context.Context, frame sdk.Frame) workerWrite {
	work := workerWrite{frame: frame, done: make(chan error, 1)}
	if frame.Kind != "ack" {
		work.ctx, work.writeClaimed = ctx, new(atomic.Bool)
	}
	return work
}

func (c *workerClient) reserveWrite(ctx context.Context, probe chan *workerWriteReservation) (*workerWriteReservation, error) {
	select {
	case c.writes <- workerWrite{probe: probe}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.lifetime.Done():
		return nil, errors.New("extension worker stopped")
	}
	select {
	case reservation := <-probe:
		return reservation, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.lifetime.Done():
		return nil, errors.New("extension worker stopped")
	}
}

func (c *workerClient) sendPrepared(ctx context.Context, frame sdk.Frame, prepare *workerCallPreparation) error {
	encoded, err := encodeWorkerFrame(frame)
	if err != nil {
		return err
	}
	release := make(chan struct{})
	work := workerWrite{frame: frame, encoded: encoded, done: make(chan error, 1), release: release}
	probe := make(chan *workerWriteReservation, 1)
	for {
		reservation, err := c.reserveWrite(ctx, probe)
		if err != nil {
			return &OutcomeError{Outcome: "not-started", Cause: err}
		}
		committed := false
		err = prepare.admit(ctx, func() error {
			var err error
			committed, err = reservation.commit(work, func() error {
				if err := c.lifetime.Err(); err != nil {
					return err
				}
				return prepare.begin(ctx)
			})
			return err
		})
		if committed {
			close(release)
			return c.awaitWrite(ctx, work)
		}
		if err != nil {
			return &OutcomeError{Outcome: "not-started", Cause: err}
		}
	}
}

func (c *workerClient) awaitWrite(ctx context.Context, work workerWrite) error {
	select {
	case err := <-work.done:
		return err
	case <-ctx.Done():
		if work.writeClaimed != nil && work.writeClaimed.CompareAndSwap(false, true) {
			return &OutcomeError{Outcome: "not-started", Cause: ctx.Err()}
		}
		// A blocked stdin write cannot be abandoned while the process
		// continues: its partial frame could corrupt the next request.
		// A tiny ACK remains serialized by writeLoop and can finish before
		// the queued cancel; it carries no new request or authority.
		if work.frame.Kind != "ack" {
			c.fail(ctx.Err())
		}
		return ctx.Err()
	case <-c.lifetime.Done():
		err := errors.New("extension worker stopped")
		if work.writeClaimed != nil && work.writeClaimed.CompareAndSwap(false, true) {
			return &OutcomeError{Outcome: "not-started", Cause: err}
		}
		return err
	}
}

func (c *workerClient) writeLoop() {
	defer close(c.writeDone)
	var reservation *workerWriteReservation
	for {
		var work workerWrite
		var committed <-chan struct{}
		if reservation != nil {
			committed = reservation.committed
		}
		select {
		case frame := <-c.cancelFrames:
			work = workerWrite{frame: frame, done: make(chan error, 1)}
		default:
			select {
			case work = <-c.writes:
			case frame := <-c.cancelFrames:
				work = workerWrite{frame: frame, done: make(chan error, 1)}
			case <-committed:
			case <-c.lifetime.Done():
				return
			}
		}
		if work.probe != nil {
			if reservation == nil {
				reservation = &workerWriteReservation{committed: make(chan struct{})}
			}
			work.probe <- reservation
			continue
		}
		if reservation != nil {
			admitted := reservation.retire()
			reservation = nil
			if admitted.done != nil {
				select {
				case <-admitted.release:
				case <-c.lifetime.Done():
					return
				}
				if !c.writeFrame(admitted) {
					return
				}
			}
		}
		if work.done != nil && !c.writeFrame(work) {
			return
		}
	}
}

func encodeWorkerFrame(frame sdk.Frame) ([]byte, error) {
	b, err := json.Marshal(frame)
	if err != nil || len(b) > sdk.MaxFrameBytes {
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension frame exceeds its contract")}
	}
	return append(b, '\n'), nil
}

func (c *workerClient) writeFrame(work workerWrite) bool {
	b := work.encoded
	if b == nil {
		var err error
		b, err = encodeWorkerFrame(work.frame)
		if err != nil {
			work.done <- err
			return true
		}
	}
	if work.writeClaimed != nil {
		err := work.ctx.Err()
		if err != nil || !work.writeClaimed.CompareAndSwap(false, true) {
			if err == nil {
				err = context.Canceled
			}
			work.done <- &OutcomeError{Outcome: "not-started", Cause: err}
			return true
		}
	}
	for len(b) > 0 {
		written, err := c.stdin.Write(b)
		if err == nil && written == 0 {
			err = io.ErrShortWrite
		}
		if err != nil {
			work.done <- err
			c.fail(err)
			return false
		}
		b = b[written:]
	}
	work.done <- nil
	return true
}

func (c *workerClient) read() {
	defer close(c.readDone)
	reader := bufio.NewScanner(c.stdout)
	reader.Buffer(make([]byte, 64<<10), sdk.MaxFrameBytes+1)
	for reader.Scan() {
		var frame sdk.Frame
		decoder := json.NewDecoder(bytes.NewReader(reader.Bytes()))
		decoder.DisallowUnknownFields()
		if len(reader.Bytes()) > sdk.MaxFrameBytes ||
			decoder.Decode(&frame) != nil || decoder.Decode(new(any)) != io.EOF ||
			frame.ID == "" || len(frame.ID) > 256 {
			c.fail(errors.New("invalid extension response envelope"))
			return
		}
		if frame.Kind == "host.call" {
			if !c.dispatchHost(frame) {
				c.fail(errors.New("invalid scoped host call"))
				return
			}
			continue
		}
		if frame.Kind != "event" && frame.Kind != "result" {
			c.fail(errors.New("unexpected extension response kind"))
			return
		}
		if frame.Method != "" || frame.Params != nil || frame.Auth != nil || frame.Scope != (sdk.Scope{}) ||
			frame.Parent != "" || frame.Deadline != 0 ||
			frame.Kind == "event" && (frame.Event == "" || frame.Seq == 0 || frame.Result != nil || frame.Error != nil) ||
			frame.Kind == "result" && (frame.Event != "" || frame.Seq != 0 || frame.Data != nil ||
				frame.Error != nil && frame.Result != nil) {
			c.fail(errors.New("invalid extension response fields"))
			return
		}
		c.mu.Lock()
		pending := c.pending[frame.ID]
		if pending != nil && frame.Kind == "event" {
			if pending.ended || pending.credit || frame.Seq != pending.seq+1 {
				c.mu.Unlock()
				c.fail(errors.New("invalid extension stream sequence"))
				return
			}
			pending.seq = frame.Seq
			pending.credit = true
		} else if pending != nil {
			if pending.ended || (pending.credit || pending.hostID != "" || len(pending.processHosts) != 0) && !pending.canceled && frame.Error == nil {
				c.mu.Unlock()
				c.fail(errors.New("extension ended before its stream was acknowledged"))
				return
			}
			pending.ended = true
			pending.credit = false
			pending.terminal = frame.Result != nil || frame.Error != nil && !c.earlyError
		}
		c.mu.Unlock()
		if pending == nil {
			c.fail(errors.New("extension response has no live call"))
			return
		}
		if frame.Kind == "result" {
			endStorageCall(pending.ctx)
		}
		select {
		case pending.frames <- frame:
		default:
			c.fail(errors.New("extension exceeded stream credit"))
			return
		}
	}
	c.fail(reader.Err())
}

func (c *workerClient) dispatchHost(frame sdk.Frame) bool {
	if c.hostCall == nil || frame.Method == "" || frame.Result != nil || frame.Error != nil ||
		frame.Auth != nil ||
		frame.Event != "" || frame.Data != nil || frame.Seq != 0 ||
		(!strings.HasPrefix(frame.Method, "state.") && frame.Method != sdk.MethodCatalogRead && frame.Method != sdk.MethodMemorySearch &&
			frame.Method != sdk.MethodDiagnosticEmit && frame.Method != sdk.MethodNetworkProxy &&
			frame.Method != sdk.MethodAccountReplace && frame.Method != sdk.MethodComponentCall && frame.Method != sdk.MethodRequestRecover &&
			frame.Method != sdk.MethodUsageInvalidate && !sdk.IsStorageHostMethod(frame.Method) &&
			!sdk.IsOwnCallHostMethod(frame.Method) && !sdk.IsAccountTaskHostMethod(frame.Method) &&
			!sdk.IsProcessHostMethod(frame.Method)) {
		return false
	}
	lane := sdk.ProcessLaneCount
	if sdk.IsProcessHostMethod(frame.Method) {
		var err error
		lane, err = sdk.ProcessHostLane(frame.Method, frame.Params)
		if err != nil {
			return false
		}
	}
	c.mu.Lock()
	parent := c.pending[frame.Parent]
	valid := parent != nil && !parent.ended && frame.ID != frame.Parent && c.pending[frame.ID] == nil &&
		(frame.Scope == (sdk.Scope{}) || frame.Scope == parent.scope) && c.hostCounts[lane] < sdk.MaxConcurrent
	if valid {
		if lane == sdk.ProcessLaneCount {
			valid = parent.hostID == ""
		} else {
			valid = parent.processHosts[lane] == ""
		}
	}
	if valid {
		for _, pending := range c.pending {
			if pending.hostID == frame.ID {
				valid = false
			}
			for _, id := range pending.processHosts {
				if id == frame.ID {
					valid = false
				}
			}
		}
	}
	if valid && parent.canceled {
		draining := !parent.cancelAt.IsZero()
		c.mu.Unlock()
		return draining
	}
	if valid {
		parent.hostWG.Add(1)
		c.hostCounts[lane]++
		if lane == sdk.ProcessLaneCount {
			parent.hostID = frame.ID
		} else {
			if parent.processHosts == nil {
				parent.processHosts = make(map[int]string)
			}
			parent.processHosts[lane] = frame.ID
		}
	}
	c.mu.Unlock()
	if !valid {
		return false
	}
	c.hostWG.Add(1)
	go func() {
		defer c.hostWG.Done()
		defer parent.hostWG.Done()
		var result any
		err := parent.ctx.Err()
		if err == nil {
			result, err = c.hostCall(parent.ctx, parent.scope, frame.Method, frame.Params)
		}
		reply := sdk.Frame{Kind: "host.result", ID: frame.ID, Parent: frame.Parent}
		if err != nil {
			reply.Error = &sdk.Error{Code: "host_error", Message: "scoped host operation failed"}
		} else {
			reply.Result, err = json.Marshal(result)
			if err != nil {
				reply.Error = &sdk.Error{Code: "host_error", Message: "invalid host operation result"}
			}
		}
		c.mu.Lock()
		c.hostCounts[lane]--
		if lane == sdk.ProcessLaneCount {
			parent.hostID = ""
		} else {
			delete(parent.processHosts, lane)
		}
		ended := parent.ended
		c.mu.Unlock()
		if ended || parent.ctx.Err() != nil {
			return
		}
		if processParent(parent.ctx) != nil {
			replyCtx, cancelReply := context.WithTimeout(parent.ctx, 15*time.Second)
			defer cancelReply()
			_ = c.send(replyCtx, reply)
		} else {
			_ = c.write(reply)
		}
	}()
	return true
}

func (c *workerClient) call(ctx context.Context, method string, scope sdk.Scope, auth json.RawMessage,
	params any, consume func(Event) error,
) (json.RawMessage, error) {
	return c.callWithAdmission(ctx, method, scope, auth, params, consume, nil, nil)
}

func (c *workerClient) callWithAdmission(ctx context.Context, method string, scope sdk.Scope, auth json.RawMessage,
	params any, consume func(Event) error, admit workerAdmission, prepare *workerCallPreparation,
) (json.RawMessage, error) {
	class := c.regularSlots
	switch method {
	case sdk.MethodInboundResponse:
		class = c.responseSlots
	case sdk.MethodDescribe, sdk.MethodOperationStatus, sdk.MethodOperationCancel,
		sdk.MethodComponentConfigure, sdk.MethodComponentReady, sdk.MethodComponentStop:
		class = c.controlSlots
	}
	select {
	case class <- struct{}{}:
		defer func() { <-class }()
	case <-ctx.Done():
		return nil, &OutcomeError{Outcome: "not-started", Cause: ctx.Err()}
	case <-c.done:
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension worker stopped")}
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, &OutcomeError{Outcome: "not-started", Cause: ctx.Err()}
	case <-c.done:
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension worker stopped")}
	}
	if err := ctx.Err(); err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	var release func(bool)
	if admit != nil {
		release, err = admit(ctx)
		if err != nil {
			return nil, &OutcomeError{Outcome: "not-started", Cause: err}
		}
	}
	var pending *pendingCall
	dispatched, retained := false, false
	defer func() {
		if !retained {
			c.finishAdmission(pending, release, dispatched)
		}
	}()
	if prepare != nil {
		auth, err = prepare.auth(ctx)
		if err != nil {
			return nil, &OutcomeError{Outcome: "not-started", Cause: err}
		}
	}
	id := strconv.FormatUint(c.next.Add(1), 10)
	if parent := processParent(ctx); parent != nil {
		parent.mu.Lock()
		if parent.bound && parent.parentID == "" && (method == sdk.MethodModuleCall || method == sdk.MethodToolCall) {
			parent.parentID = id
		} else {
			ctx = context.WithValue(ctx, processContextKey{}, (*ownedProcess)(nil))
		}
		parent.mu.Unlock()
	}
	callbackCtx, stopCallbacks := context.WithCancel(ctx)
	defer stopCallbacks()
	pending = &pendingCall{ctx: callbackCtx, scope: scope, frames: make(chan sdk.Frame, 2)}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension worker stopped")}
	}
	c.pending[id] = pending
	c.mu.Unlock()
	trackStorageCall(ctx)
	successfulProcessResult := false
	defer func() {
		c.mu.Lock()
		pending.canceled = true
		if !retained {
			pending.ended = true
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if !successfulProcessResult {
			stopCallbacks()
		}
		if parent := processParent(ctx); parent != nil {
			parent.close("parent-finished")
		}
		pending.hostWG.Wait()
	}()
	frame := sdk.Frame{Kind: "call", ID: id, Method: method, Scope: scope, Auth: auth, Params: payload}
	if deadline, ok := ctx.Deadline(); ok {
		frame.Deadline = deadline.UnixMilli()
	}
	if prepare == nil {
		err = c.send(ctx, frame)
	} else {
		err = c.sendPrepared(ctx, frame, prepare)
	}
	if err != nil {
		var outcome *OutcomeError
		if errors.As(err, &outcome) && outcome.Outcome == "not-started" {
			return nil, err
		}
		dispatched = true
		return nil, &OutcomeError{Outcome: "uncertain", Cause: err}
	}
	dispatched = true
	for {
		var reply sdk.Frame
		select {
		case reply = <-pending.frames:
		case <-ctx.Done():
			// The worker cannot keep an unresponsive canceled operation
			// forever. Retire it after a bounded grace period.
			retained = c.cancelPending(id, pending, release)
			return nil, ctx.Err()
		case <-c.readDone:
			select {
			case reply = <-pending.frames:
			default:
				return nil, &OutcomeError{Outcome: "uncertain", Cause: errors.New("extension worker exited")}
			}
		}
		if reply.Kind == "event" {
			if consume != nil {
				if err := consume(Event{Name: reply.Event, Data: reply.Data}); err != nil {
					retained = c.cancelPending(id, pending, release)
					return nil, err
				}
			}
			c.mu.Lock()
			pending.credit = false
			ended := pending.ended
			c.mu.Unlock()
			if ended {
				continue
			}
			if err := c.send(ctx, sdk.Frame{Kind: "ack", ID: id, Seq: reply.Seq}); err != nil {
				if ctx.Err() != nil {
					retained = c.cancelPending(id, pending, release)
					return nil, ctx.Err()
				}
				return nil, &OutcomeError{Outcome: "uncertain", Cause: err}
			}
			continue
		}
		if reply.Error != nil {
			return nil, reply.Error
		}
		if len(reply.Result) > sdk.MaxFrameBytes || !json.Valid(reply.Result) {
			return nil, errors.New("invalid extension result")
		}
		successfulProcessResult = processParent(ctx) != nil
		return reply.Result, nil
	}
}

func (c *workerClient) finishAdmission(pending *pendingCall, release func(bool), dispatched bool) {
	if release == nil {
		return
	}
	if !dispatched {
		release(true)
		return
	}
	c.mu.Lock()
	terminal := pending.terminal
	c.mu.Unlock()
	if terminal {
		release(true)
	} else if c.processExit == nil {
		release(false)
	} else {
		c.fail(errors.New("worker stopped before confirming an admitted subscription call"))
		go func() {
			<-c.processExit
			release(true)
		}()
	}
}

func (c *workerClient) cancelPending(id string, pending *pendingCall, release func(bool)) bool {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return false
	}
	pending.canceled = true
	pending.cancelAt = time.Now()
	c.cancelWG.Add(1)
	c.mu.Unlock()
	select {
	case c.cancelFrames <- sdk.Frame{Kind: "cancel", ID: id}:
	default:
		c.fail(errors.New("extension cancellation queue exceeded its bound"))
	}
	go func() {
		defer c.cancelWG.Done()
		c.retireCanceled(id, pending)
		if release != nil {
			pending.hostWG.Wait()
			c.finishAdmission(pending, release, true)
		}
	}()
	return true
}

func (c *workerClient) retireCanceled(id string, pending *pendingCall) {
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	timer := time.NewTimer(time.Until(pending.cancelAt.Add(2 * time.Second)))
	defer timer.Stop()
	for {
		select {
		case frame := <-pending.frames:
			if frame.Kind == "result" {
				return
			}
		case <-c.readDone:
			return
		case <-timer.C:
			c.fail(fmt.Errorf("extension did not end canceled call %s", id))
			return
		}
	}
}

func (c *workerClient) close() {
	c.cancel()
	_ = c.stdin.Close()
	_ = c.stdout.Close()
	<-c.done
}
