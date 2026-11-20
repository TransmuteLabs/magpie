package extension

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/yetone/magpie/internal/proc"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type processPipe struct {
	chunks chan []byte
	tail   []byte
	next   uint64
	err    error
}

type ownedProcess struct {
	lease        *Lease
	scope        sdk.Scope
	launch       OwnedProcessLaunch
	executable   processFileIdentity
	directory    os.FileInfo
	ctx          context.Context
	cancel       context.CancelFunc
	ioctx        context.Context
	stopIO       context.CancelFunc
	mu           sync.Mutex
	openMu       sync.Mutex
	ioMu         sync.Mutex
	ioClosed     bool
	ioWG         sync.WaitGroup
	pumpWG       sync.WaitGroup
	tree         *proc.Tree
	stdin        *os.File
	outputs      [2]*os.File
	pipes        [2]processPipe
	writeSeq     uint64
	writeIDs     map[string]bool
	handle       string
	incarnation  string
	component    string
	parentID     string
	started      time.Time
	reason       string
	stdinBytes   uint64
	stdoutBytes  uint64
	stderrBytes  uint64
	exitCode     *int
	joined       bool
	receiptErr   error
	bound        bool
	closeOnce    sync.Once
	closeRequest chan struct{}
	closed       chan struct{}
	waitDone     chan struct{}
	watchDone    chan struct{}
	stopLease    func() bool
	stopManager  func() bool
}

func newOwnedProcess(ctx context.Context, lease *Lease, scope sdk.Scope, launch OwnedProcessLaunch,
	executable processFileIdentity, directory os.FileInfo,
) (*ownedProcess, error) {
	handle, err := randomID()
	if err != nil {
		return nil, err
	}
	incarnation, err := randomID()
	if err != nil {
		return nil, err
	}
	parentCtx, cancel := context.WithCancel(ctx)
	ioctx, stopIO := context.WithCancel(parentCtx)
	parent := &ownedProcess{
		lease: lease, scope: scope, launch: launch, executable: executable, directory: directory,
		ctx: parentCtx, cancel: cancel, ioctx: ioctx, stopIO: stopIO,
		handle: handle, incarnation: incarnation, started: time.Now(), writeIDs: make(map[string]bool),
		closeRequest: make(chan struct{}), closed: make(chan struct{}), watchDone: make(chan struct{}),
	}
	if err := parent.check(parentCtx); err != nil {
		cancel()
		stopIO()
		return nil, err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		cancel()
		stopIO()
		return nil, errors.New("owned process revocation observer is unavailable")
	}
	paths := append([]string{
		lease.manager.root,
		filepath.Join(lease.manager.root, "private", scope.Plugin),
		filepath.Dir(executable.path), filepath.Dir(executable.original), launch.Directory,
	}, launch.WatchPaths...)
	paths = append(paths, filepath.Join(lease.manager.root, "private", scope.Plugin, "state"))
	for _, path := range paths {
		if err := watcher.Add(path); err != nil {
			watcher.Close()
			cancel()
			stopIO()
			return nil, errors.New("owned process authority observation is unavailable")
		}
	}
	manager := lease.manager
	manager.mu.Lock()
	manager.startLifecycleLocked()
	if manager.closed || lease.closed.Load() || lease.ctx.Err() != nil || parentCtx.Err() != nil || len(manager.processes) >= 4 {
		manager.mu.Unlock()
		watcher.Close()
		cancel()
		stopIO()
		return nil, errors.New("owned process admission is unavailable or full")
	}
	if manager.processes == nil {
		manager.processes = make(map[*ownedProcess]struct{})
	}
	manager.processes[parent] = struct{}{}
	manager.processWG.Add(1)
	manager.mu.Unlock()
	parent.stopLease = context.AfterFunc(lease.ctx, func() { parent.revoke("lease-closed") })
	parent.stopManager = context.AfterFunc(manager.startCtx, func() { parent.revoke("manager-closed") })
	go parent.shutdown()
	go func() {
		defer close(parent.watchDone)
		defer watcher.Close()
		for {
			select {
			case <-parent.ctx.Done():
				parent.requestClose("canceled")
				return
			case event, open := <-watcher.Events:
				if !open {
					parent.revoke("observer-closed")
					return
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
					if err := parent.check(parent.ctx); err != nil {
						parent.revoke("authority-changed")
						return
					}
				}
			case <-watcher.Errors:
				parent.revoke("observer-failed")
				return
			}
		}
	}()
	return parent, nil
}

func (parent *ownedProcess) requestClose(reason string) {
	parent.closeOnce.Do(func() {
		parent.mu.Lock()
		parent.reason = reason
		parent.mu.Unlock()
		close(parent.closeRequest)
	})
}

func (parent *ownedProcess) revoke(reason string) {
	parent.requestClose(reason)
	parent.cancel()
}

func (parent *ownedProcess) close(reason string) {
	parent.requestClose(reason)
	<-parent.closed
}

func (parent *ownedProcess) finish() {
	parent.close("parent-finished")
	parent.cancel()
	parent.stopLease()
	parent.stopManager()
	<-parent.watchDone
	parent.lease.manager.mu.Lock()
	delete(parent.lease.manager.processes, parent)
	parent.lease.manager.mu.Unlock()
	parent.lease.manager.processWG.Done()
}

func (parent *ownedProcess) shutdown() {
	<-parent.closeRequest
	parent.openMu.Lock()
	parent.ioMu.Lock()
	parent.ioClosed = true
	parent.ioMu.Unlock()
	parent.stopIO()
	if parent.stdin != nil {
		parent.stdin.Close()
	}
	if parent.tree != nil {
		stopped := parent.tree.Stop(200 * time.Millisecond)
		for _, output := range parent.outputs {
			output.Close()
		}
		<-parent.waitDone
		<-stopped
	}
	parent.openMu.Unlock()
	parent.ioWG.Wait()
	parent.pumpWG.Wait()
	receiptErr := parent.journalJoined()
	parent.mu.Lock()
	parent.joined, parent.receiptErr = true, receiptErr
	parent.mu.Unlock()
	parent.observe("joined")
	close(parent.closed)
}

func (parent *ownedProcess) beginIO() error {
	parent.ioMu.Lock()
	defer parent.ioMu.Unlock()
	if parent.ioClosed || parent.ioctx.Err() != nil {
		return errors.New("owned process pipe was revoked")
	}
	parent.ioWG.Add(1)
	return nil
}

func (parent *ownedProcess) open(ctx context.Context, recipe string) (sdk.ProcessOpenResult, error) {
	parent.openMu.Lock()
	defer parent.openMu.Unlock()
	if recipe != parent.launch.Recipe.ID || parent.tree != nil {
		return sdk.ProcessOpenResult{}, errors.New("owned process recipe is foreign or already admitted")
	}
	if err := parent.check(ctx); err != nil {
		return sdk.ProcessOpenResult{}, err
	}
	select {
	case <-parent.closeRequest:
		return sdk.ProcessOpenResult{}, errors.New("owned process was closed")
	default:
	}
	release := func() {}
	if parent.launch.Admit != nil {
		var err error
		release, err = parent.launch.Admit(ctx)
		if err != nil {
			return sdk.ProcessOpenResult{}, err
		}
		if release == nil {
			return sdk.ProcessOpenResult{}, errors.New("owned process admission guard is missing")
		}
	}
	defer release()
	if err := parent.check(ctx); err != nil {
		return sdk.ProcessOpenResult{}, err
	}
	if err := parent.journal("spawn-intent", "", 0, 0, true); err != nil {
		return sdk.ProcessOpenResult{}, err
	}
	command := proc.Command(parent.executable.path, parent.launch.Recipe.Argv...)
	command.Dir, command.Env = parent.launch.Directory, append([]string{}, parent.launch.Env...)
	var files []*os.File
	defer func() {
		for _, file := range files {
			file.Close()
		}
	}()
	input, stdin, err := os.Pipe()
	if err != nil {
		return sdk.ProcessOpenResult{}, err
	}
	files = append(files, input, stdin)
	command.Stdin = input
	var outputs [2]*os.File
	for index := range outputs {
		reader, writer, err := os.Pipe()
		if err != nil {
			return sdk.ProcessOpenResult{}, err
		}
		files = append(files, reader, writer)
		outputs[index] = reader
		if index == 0 {
			command.Stdout = writer
		} else {
			command.Stderr = writer
		}
	}
	if err := parent.check(ctx); err != nil {
		return sdk.ProcessOpenResult{}, err
	}
	tree, err := proc.StartTree(command)
	if err != nil {
		return sdk.ProcessOpenResult{}, errors.New("owned process could not start")
	}
	parent.tree, parent.stdin, parent.outputs = tree, stdin, outputs
	parent.waitDone = make(chan struct{})
	for index, output := range outputs {
		parent.pipes[index] = processPipe{chunks: make(chan []byte, 2), next: 1}
		parent.pumpWG.Add(1)
		go parent.pump(index, output)
	}
	go func() {
		_ = tree.Wait()
		parent.mu.Lock()
		code := command.ProcessState.ExitCode()
		parent.exitCode = &code
		parent.mu.Unlock()
		close(parent.waitDone)
	}()
	for _, file := range files {
		if file != stdin && file != outputs[0] && file != outputs[1] {
			file.Close()
		}
	}
	files = nil
	if err := parent.journal("spawned", "", 0, 0, false); err != nil {
		parent.revoke("receipt-failed")
		return sdk.ProcessOpenResult{}, err
	}
	parent.observe("spawned")
	return sdk.ProcessOpenResult{Handle: parent.handle, Incarnation: parent.incarnation,
		Limits: sdk.ProcessLimits{MaxChunkBytes: sdk.MaxChunkBytes, QueueChunks: 2}}, nil
}

func (parent *ownedProcess) pump(index int, output *os.File) {
	defer parent.pumpWG.Done()
	pipe := &parent.pipes[index]
	defer close(pipe.chunks)
	for {
		buffer := make([]byte, sdk.MaxChunkBytes)
		count, err := output.Read(buffer)
		if count > 0 {
			parent.mu.Lock()
			if index == 0 {
				parent.stdoutBytes += uint64(count)
			} else {
				parent.stderrBytes += uint64(count)
			}
			parent.mu.Unlock()
			select {
			case pipe.chunks <- buffer[:count]:
			case <-parent.ioctx.Done():
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				pipe.err = errors.New("owned process pipe failed")
			}
			return
		}
	}
}

func (parent *ownedProcess) read(ctx context.Context, request sdk.ProcessReadRequest) (sdk.ProcessReadResult, error) {
	if err := parent.beginIO(); err != nil {
		return sdk.ProcessReadResult{}, err
	}
	defer parent.ioWG.Done()
	index := 0
	if request.Pipe == "stderr" {
		index = 1
	}
	pipe := &parent.pipes[index]
	if request.Seq != strconv.FormatUint(pipe.next, 10) {
		return sdk.ProcessReadResult{}, errors.New("owned process read sequence cannot be replayed")
	}
	eof := false
	if len(pipe.tail) == 0 {
		select {
		case chunk, open := <-pipe.chunks:
			if !open {
				if pipe.err != nil {
					return sdk.ProcessReadResult{}, pipe.err
				}
				eof = true
			} else {
				pipe.tail = chunk
			}
		case <-ctx.Done():
			return sdk.ProcessReadResult{}, ctx.Err()
		case <-parent.ioctx.Done():
			return sdk.ProcessReadResult{}, errors.New("owned process output was revoked")
		}
	}
	if err := parent.check(ctx); err != nil || parent.ioctx.Err() != nil {
		return sdk.ProcessReadResult{}, errors.New("owned process publication was revoked")
	}
	count := min(request.MaxBytes, len(pipe.tail))
	result := sdk.ProcessReadResult{Seq: request.Seq, Bytes: append([]byte{}, pipe.tail[:count]...), EOF: eof}
	pipe.tail = pipe.tail[count:]
	pipe.next++
	return result, nil
}

func (parent *ownedProcess) write(ctx context.Context, request sdk.ProcessWriteRequest) (sdk.ProcessWriteResult, error) {
	if err := parent.beginIO(); err != nil {
		return sdk.ProcessWriteResult{}, err
	}
	defer parent.ioWG.Done()
	operation, err := parent.lease.manager.store.OperationContext(ctx, parent.scope.Plugin, parent.scope.Operation)
	if ctx.Err() != nil {
		return sdk.ProcessWriteResult{}, ctx.Err()
	}
	if err != nil || operation.Scope != parent.scope || operation.State != "accepted" {
		return sdk.ProcessWriteResult{}, errors.New("owned process write requires durable foreground acceptance")
	}
	parent.mu.Lock()
	if request.Seq != strconv.FormatUint(parent.writeSeq+1, 10) || parent.writeIDs[request.WriteID] || len(parent.writeIDs) >= 4096 {
		parent.mu.Unlock()
		return sdk.ProcessWriteResult{}, errors.New("owned process writes cannot be replayed")
	}
	parent.writeSeq++
	seq := parent.writeSeq
	parent.writeIDs[request.WriteID] = true
	parent.mu.Unlock()
	if err := parent.journal("write-intent", request.WriteID, seq, 0, false); err != nil {
		return sdk.ProcessWriteResult{}, err
	}
	if err := parent.check(ctx); err != nil {
		return sdk.ProcessWriteResult{}, err
	}
	count, writeErr := parent.stdin.Write(request.Bytes)
	parent.mu.Lock()
	parent.stdinBytes += uint64(count)
	parent.mu.Unlock()
	state := "written"
	if writeErr != nil || count != len(request.Bytes) {
		state = "uncertain"
	}
	if err := parent.journal(state, request.WriteID, seq, count, false); err != nil {
		return sdk.ProcessWriteResult{}, err
	}
	return sdk.ProcessWriteResult{Written: count, Delivery: state}, nil
}

func (manager *Manager) processHostCall(ctx context.Context, scope sdk.Scope, method string, raw json.RawMessage,
	authority hostAuthority,
) (any, error) {
	parent := processParent(ctx)
	if parent == nil || parent.lease.manager != manager || parent.scope != scope ||
		authority.method != sdk.MethodToolCall && authority.method != sdk.MethodModuleCall {
		return nil, errors.New("process callbacks require their original host owner")
	}
	component, owned := ctx.Value(componentAuthorityKey{}).(componentAuthority)
	if !owned || component.component != parent.component || component.effect != "write" {
		return nil, errors.New("process callbacks cannot delegate their component authority")
	}
	if _, err := sdk.ProcessHostLane(method, raw); err != nil {
		return nil, err
	}
	if err := parent.check(ctx); err != nil {
		parent.revoke("authority-changed")
		return nil, err
	}
	if method == sdk.MethodProcessOpen {
		var request sdk.ProcessOpenRequest
		_ = json.Unmarshal(raw, &request)
		return parent.open(ctx, request.RecipeID)
	}
	var request sdk.ProcessHandleRequest
	var envelope struct {
		Handle string `json:"handle"`
	}
	_ = json.Unmarshal(raw, &envelope)
	request.Handle = envelope.Handle
	parent.openMu.Lock()
	valid := parent.tree != nil && request.Handle == parent.handle
	parent.openMu.Unlock()
	if !valid {
		return nil, errors.New("process handle is foreign or stale")
	}
	switch method {
	case sdk.MethodProcessRead:
		var request sdk.ProcessReadRequest
		_ = json.Unmarshal(raw, &request)
		return parent.read(ctx, request)
	case sdk.MethodProcessWrite:
		var request sdk.ProcessWriteRequest
		_ = json.Unmarshal(raw, &request)
		return parent.write(ctx, request)
	case sdk.MethodProcessStdinEOF:
		err := parent.stdin.Close()
		if errors.Is(err, os.ErrClosed) {
			err = nil
		}
		return struct{}{}, err
	case sdk.MethodProcessClose:
		parent.close("closed")
		parent.mu.Lock()
		result := sdk.ProcessCloseResult{Joined: parent.joined, ExitCode: parent.exitCode, Reason: parent.reason}
		parent.mu.Unlock()
		return result, nil
	}
	return nil, errors.New("unknown owned process callback")
}

func (lease *Lease) closeOwnedProcesses() {
	lease.manager.mu.Lock()
	var parents []*ownedProcess
	for parent := range lease.manager.processes {
		if parent.lease == lease {
			parents = append(parents, parent)
		}
	}
	lease.manager.mu.Unlock()
	for _, parent := range parents {
		parent.revoke("lease-closed")
		parent.close("lease-closed")
	}
}

func (manager *Manager) revokeOwnedProcesses(current map[string]Installed, configs map[string]string) {
	manager.mu.Lock()
	var revoke []*ownedProcess
	for parent := range manager.processes {
		entry, found := current[parent.scope.Plugin]
		if !found || entry.Revision != parent.lease.Revision() || configs[parent.scope.Plugin] != parent.lease.ConfigVersion() {
			revoke = append(revoke, parent)
		}
	}
	manager.mu.Unlock()
	for _, parent := range revoke {
		parent.revoke("module-changed")
	}
}
