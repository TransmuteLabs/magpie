// Package extension installs and supervises versioned external modules.
// Vendor codecs and credentials never become gateway dispatch switches.
package extension

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/netproxy"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type managedWorker struct {
	mu       sync.Mutex
	entry    Installed
	client   *workerClient
	active   int
	retiring bool
	idle     chan struct{}
	starting chan struct{}
	config   sdk.ConfigSnapshot
}

type Manager struct {
	mu         sync.Mutex
	reloadMu   sync.Mutex
	root       string
	catalog    *Catalog
	store      *Store
	workers    map[string]*managedWorker
	closed     bool
	closeDone  chan struct{}
	closeErr   error
	startCtx   context.Context
	stopStarts context.CancelFunc
	startWG    sync.WaitGroup
	retireWG   sync.WaitGroup
	// Bun is an explicitly selected runtime executable; empty uses discovery.
	Bun string
	// ReleaseClient optionally supplies the client used for GitHub packages.
	// Set it before using the manager; nil uses Magpie's proxy-aware transport.
	ReleaseClient *http.Client
}

func NewManager(root string) *Manager {
	if root != "" {
		if absolute, err := filepath.Abs(root); err == nil {
			root = absolute
		}
	}
	return &Manager{
		root: root, catalog: NewCatalog(root), store: NewStore(root),
		workers: make(map[string]*managedWorker),
	}
}

// Metadata reads allocate no lifecycle channels. In particular, a manager
// discovered in a synctest bubble is not left with bubble-owned channels for
// an unrelated later request to close.
func (m *Manager) startLifecycleLocked() {
	if m.startCtx == nil {
		m.startCtx, m.stopStarts = context.WithCancel(context.Background())
		m.closeDone = make(chan struct{})
	}
}

var defaults struct {
	sync.Mutex
	manager *Manager
	config  string
}

// Default follows appdir's already selected portable/installed configuration.
// Changing a test's home never reuses a worker from the previous home.
func Default() *Manager {
	config := appdir.Config()
	defaults.Lock()
	if defaults.config == config && defaults.manager != nil && !defaults.manager.isClosed() {
		selected := defaults.manager
		defaults.Unlock()
		return selected
	}
	defaults.Unlock()
	root := filepath.Join(canonicalConfigPath(config), "extensions")
	defaults.Lock()
	var old *Manager
	if defaults.manager == nil || defaults.manager.root != root || defaults.manager.isClosed() {
		old = defaults.manager
		defaults.manager = NewManager(root)
	}
	defaults.config = config
	selected := defaults.manager
	defaults.Unlock()
	if old != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = old.Close(ctx)
	}
	return selected
}

func canonicalConfigPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	current := absolute
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved
		}
		if !errors.Is(err, os.ErrNotExist) {
			return absolute
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *Manager) Store() *Store { return m.store }

func (m *Manager) Namespace() string { return m.root }

// Running describes this manager's process, not workers in another Magpie.
func (m *Manager) Running(id string) bool {
	m.mu.Lock()
	worker := m.workers[id]
	m.mu.Unlock()
	if worker == nil {
		return false
	}
	worker.mu.Lock()
	client := worker.client
	worker.mu.Unlock()
	return client != nil && client.alive()
}

func (m *Manager) List() ([]Installed, error) { return m.catalog.List() }

func (m *Manager) Manifest(id string) (sdk.Manifest, error) {
	entries, err := m.List()
	if err != nil {
		return sdk.Manifest{}, err
	}
	for _, entry := range entries {
		if entry.Manifest.ID == id && !entry.Disabled {
			return entry.Manifest, nil
		}
	}
	return sdk.Manifest{}, errors.New("extension is not enabled")
}

func (m *Manager) Install(ctx context.Context, source string) (Installed, error) {
	return m.install(ctx, source, installOptions{})
}

// Update follows a saved release source. Pinned and local distributions stay
// at the version the user explicitly installed; Install selects another pin.
func (m *Manager) Update(ctx context.Context, id string) (Installed, error) {
	if err := ctx.Err(); err != nil {
		return Installed{}, err
	}
	entries, err := m.List()
	if err != nil {
		return Installed{}, err
	}
	for _, entry := range entries {
		if entry.Manifest.ID != id {
			continue
		}
		if entry.Source == nil || strings.Contains(entry.Source.Reference, "@") {
			return entry, nil
		}
		return m.install(ctx, entry.Source.Reference, installOptions{expected: &entry})
	}
	return Installed{}, fmt.Errorf("unknown extension %q", id)
}

func (m *Manager) install(ctx context.Context, source string, options installOptions) (Installed, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Installed{}, errors.New("extension manager is closed")
	}
	m.startLifecycleLocked()
	m.startWG.Add(1)
	m.mu.Unlock()
	defer m.startWG.Done()
	installCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.startCtx, cancel)
	defer func() { stop(); cancel() }()
	ctx = installCtx
	if strings.HasPrefix(source, "github:") {
		client := m.ReleaseClient
		if client == nil {
			client = releaseHTTPClient()
			defer client.CloseIdleConnections()
		}
		prepared, cleanup, err := PrepareRelease(netproxy.Downloads(ctx), client, source)
		if err != nil {
			return Installed{}, err
		}
		defer cleanup()
		source, options.source = prepared.Directory, &prepared.Source
	}
	var checkedConfig *sdk.ConfigSnapshot
	var checkedModuleID string
	priorCheck := options.commitCheck
	options.commitCheck = func(ctx context.Context) error {
		if priorCheck != nil {
			if err := priorCheck(ctx); err != nil {
				return err
			}
		}
		if checkedConfig == nil {
			return nil
		}
		config, err := m.readModuleConfig(checkedModuleID)
		if err != nil {
			return err
		}
		if config.Version != checkedConfig.Version {
			return ErrCatalogConflict
		}
		return nil
	}
	entry, err := m.catalog.install(ctx, source, func(ctx context.Context, candidate Installed) error {
		entries, err := m.List()
		if err != nil {
			return err
		}
		if err := checkOwnership(entries, candidate); err != nil {
			return err
		}
		handshake, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		config, err := m.runtimeModuleConfig(candidate.Manifest)
		if err != nil {
			return err
		}
		if candidate.Manifest.Contract.Version.Minor >= 1 {
			checkedConfig, checkedModuleID = &config, candidate.Manifest.ID
		}
		worker, err := startWorker(handshake, m.root, candidate, m.Bun, m.hostCall)
		if err != nil {
			return err
		}
		group := newComponentGroup(m, candidate, config)
		if err := group.prepare(handshake, worker); err != nil {
			_ = group.Close(context.Background())
			return err
		}
		for _, service := range candidate.Manifest.Services {
			if _, err := group.ensureService(handshake, service.ID); err != nil {
				_ = group.Close(context.Background())
				return err
			}
		}
		return group.Close(context.Background())
	}, options)
	if err == nil {
		err = m.Reload(ctx)
		notifyChanged()
	}
	return entry, err
}

func checkOwnership(entries []Installed, candidate Installed) error {
	if candidate.Disabled {
		return nil
	}
	paths, providers, agents, sessions := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, entry := range entries {
		if entry.Disabled || entry.Manifest.ID == candidate.Manifest.ID {
			continue
		}
		for _, inbound := range entry.Manifest.Inbounds {
			paths[inbound.Path] = true
		}
		for _, provider := range entry.Manifest.Providers {
			providers[provider.ID] = true
		}
		for _, agent := range entry.Manifest.Agents {
			agents[agent.ID] = true
		}
		for _, session := range entry.Manifest.Sessions {
			sessions[session.ID] = true
		}
	}
	for _, inbound := range candidate.Manifest.Inbounds {
		if paths[inbound.Path] {
			return errors.New("extension inbound path is already owned")
		}
	}
	for _, provider := range candidate.Manifest.Providers {
		if providers[provider.ID] {
			return errors.New("extension provider is already owned")
		}
	}
	for _, agent := range candidate.Manifest.Agents {
		if agents[agent.ID] {
			return errors.New("extension agent is already owned")
		}
	}
	for _, session := range candidate.Manifest.Sessions {
		if sessions[session.ID] {
			return errors.New("extension session format is already owned")
		}
	}
	return nil
}

func (m *Manager) SetEnabled(ctx context.Context, id string, enabled bool) error {
	if m.isClosed() {
		return errors.New("extension manager is closed")
	}
	if err := m.catalog.SetEnabled(ctx, id, enabled); err != nil {
		return err
	}
	err := m.Reload(ctx)
	notifyChanged()
	return err
}

func (m *Manager) Remove(ctx context.Context, id string) error {
	if m.isClosed() {
		return errors.New("extension manager is closed")
	}
	if err := m.catalog.Remove(ctx, id); err != nil {
		return err
	}
	err := m.Reload(ctx)
	notifyChanged()
	return err
}

// Restart publishes a verified new package generation. Other Magpie
// processes observe the revision through the same catalog as installation.
func (m *Manager) Restart(ctx context.Context, id string) error {
	entries, err := m.List()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Manifest.ID == id {
			if err := m.catalog.verify(ctx, entry); err != nil {
				return err
			}
			_, err = m.install(ctx, filepath.Join(m.root, entry.Directory), installOptions{
				source: entry.Source, expected: &entry, force: true,
			})
			return err
		}
	}
	return errors.New("extension is not installed")
}

func (m *Manager) Reload(ctx context.Context) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := m.List()
	if err != nil {
		return err
	}
	current := make(map[string]Installed, len(entries))
	configs := make(map[string]string, len(entries))
	for _, entry := range entries {
		if !entry.Disabled {
			current[entry.Manifest.ID] = entry
			config, err := m.runtimeModuleConfig(entry.Manifest)
			if err != nil {
				return err
			}
			configs[entry.Manifest.ID] = config.Version
		}
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("extension manager is closed")
	}
	var retiring []*managedWorker
	for id, worker := range m.workers {
		entry, found := current[id]
		if !found || entry.Revision != worker.entry.Revision || configs[id] != worker.config.Version {
			delete(m.workers, id)
			retiring = append(retiring, worker)
		}
	}
	m.retireWG.Add(len(retiring))
	m.mu.Unlock()
	// New lookups have already lost these generations. An active call keeps
	// its own worker until it drains; retiring never redirects its response.
	for _, worker := range retiring {
		m.retire(worker)
	}
	return ctx.Err()
}

func (m *Manager) retire(worker *managedWorker) {
	worker.mu.Lock()
	worker.retiring = true
	if worker.active == 0 {
		client := worker.client
		worker.mu.Unlock()
		if client != nil {
			_ = closeModuleClient(client)
		}
		m.retireWG.Done()
		return
	}
	idle := worker.idle
	worker.mu.Unlock()
	go func() {
		defer m.retireWG.Done()
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-idle:
		case <-timer.C:
		case <-m.startCtx.Done():
		}
		worker.mu.Lock()
		client := worker.client
		worker.mu.Unlock()
		if client != nil {
			_ = closeModuleClient(client)
		}
	}()
}

type expectedGeneration struct {
	packageRevision string
	configVersion   string
}

func (m *Manager) acquire(ctx context.Context, id string, expected *expectedGeneration) (*managedWorker, *workerClient, error) {
	if err := m.Reload(ctx); err != nil {
		return nil, nil, err
	}
	entries, err := m.List()
	if err != nil {
		return nil, nil, err
	}
	var selected *Installed
	for index := range entries {
		if entries[index].Manifest.ID == id && !entries[index].Disabled {
			selected = &entries[index]
			break
		}
	}
	if selected == nil {
		return nil, nil, errors.New("extension is not enabled")
	}
	config, err := m.runtimeModuleConfig(selected.Manifest)
	if err != nil {
		return nil, nil, err
	}
	if expected != nil && (expected.packageRevision != "" && expected.packageRevision != selected.Revision ||
		expected.configVersion != "" && expected.configVersion != config.Version) {
		return nil, nil, errors.New("module generation changed before acquisition")
	}
	if err := sdk.CheckCompatibility(selected.Manifest); err != nil {
		return nil, nil, err
	}
	return m.acquireSelected(ctx, id, selected, config)
}

func (m *Manager) acquireSelected(ctx context.Context, id string, selected *Installed, config sdk.ConfigSnapshot) (*managedWorker, *workerClient, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("extension manager is closed")
	}
	worker := m.workers[id]
	if worker != nil && (worker.entry.Revision != selected.Revision || worker.config.Version != config.Version) {
		m.mu.Unlock()
		return nil, nil, errors.New("module generation changed before cached worker admission")
	}
	if worker == nil {
		m.startLifecycleLocked()
		worker = &managedWorker{entry: *selected, config: cloneModuleConfig(config)}
		m.workers[id] = worker
	}
	m.mu.Unlock()
	for {
		worker.mu.Lock()
		if worker.retiring {
			worker.mu.Unlock()
			return nil, nil, errors.New("extension generation is draining")
		}
		if worker.client != nil && worker.client.alive() {
			if worker.active == 0 {
				worker.idle = make(chan struct{})
			}
			worker.active++
			client := worker.client
			worker.mu.Unlock()
			return worker, client, nil
		}
		if waiting := worker.starting; waiting != nil {
			worker.mu.Unlock()
			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			worker.mu.Unlock()
			return nil, nil, errors.New("extension manager is closed")
		}
		m.startWG.Add(1)
		m.mu.Unlock()
		worker.starting = make(chan struct{})
		started := worker.starting
		old := worker.client
		worker.client = nil
		worker.mu.Unlock()
		if old != nil {
			_ = closeModuleClient(old)
		}
		handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
		stop := context.AfterFunc(m.startCtx, cancel)
		err := m.catalog.verify(handshake, worker.entry)
		var client *workerClient
		if err == nil {
			client, err = startWorker(handshake, m.root, worker.entry, m.Bun, m.hostCall)
		}
		if err == nil {
			group := newComponentGroup(m, worker.entry, worker.config)
			err = group.prepare(handshake, client)
			if err == nil {
				client.group = group
			} else {
				_ = group.Close(context.Background())
			}
		}
		cancel()
		stop()
		worker.mu.Lock()
		retiring := worker.retiring
		if err == nil && !retiring {
			worker.client = client
		}
		worker.starting = nil
		close(started)
		worker.mu.Unlock()
		if retiring && client != nil {
			_ = closeModuleClient(client)
			err = errors.New("extension generation was retired during startup")
		}
		m.startWG.Done()
		if err != nil {
			return nil, nil, err
		}
	}
}

func releaseWorker(worker *managedWorker) {
	worker.mu.Lock()
	worker.active--
	if worker.active == 0 {
		close(worker.idle)
	}
	worker.mu.Unlock()
}

// Lease pins one installed worker generation across a native request's decode
// and response events. Updating a plugin cannot mix two codecs' stream state.
type Lease struct {
	manager *Manager
	worker  *managedWorker
	client  *workerClient
	once    sync.Once
	ctx     context.Context
	cancel  context.CancelFunc
	closed  atomic.Bool
	native  bool
	group   *componentGroup
}

func (m *Manager) Acquire(ctx context.Context, id string) (*Lease, error) {
	return m.acquireLease(ctx, id, nil)
}

func (m *Manager) AcquireRevision(ctx context.Context, id, packageRevision, configVersion string) (*Lease, error) {
	return m.acquireLease(ctx, id, &expectedGeneration{packageRevision: packageRevision, configVersion: configVersion})
}

func (m *Manager) acquireLease(ctx context.Context, id string, expected *expectedGeneration) (*Lease, error) {
	worker, client, err := m.acquire(ctx, id, expected)
	if err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	lifetime, cancel := context.WithCancel(ctx)
	return &Lease{manager: m, worker: worker, client: client, group: client.group, ctx: lifetime, cancel: cancel}, nil
}

// AcquireNative reserves a logical codec response as well as its process
// generation. Separate RPC credits leave room to encode a provider's output
// even when inference calls occupy every ordinary execution slot.
func (m *Manager) AcquireNative(ctx context.Context, id string) (*Lease, error) {
	lease, err := m.Acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	select {
	case lease.client.nativeSlots <- struct{}{}:
		lease.native = true
		return lease, nil
	case <-ctx.Done():
		lease.Close()
		return nil, ctx.Err()
	case <-lease.client.done:
		lease.Close()
		return nil, errors.New("extension worker stopped")
	}
}

func (l *Lease) Manifest() sdk.Manifest { return l.worker.entry.Manifest.Public() }
func (l *Lease) Revision() string       { return l.worker.entry.Revision }
func (l *Lease) ConfigVersion() string  { return l.worker.config.Version }
func (l *Lease) Close() {
	l.once.Do(func() {
		l.closed.Store(true)
		l.cancel()
		if l.native {
			<-l.client.nativeSlots
		}
		releaseWorker(l.worker)
	})
}

func (m *Manager) Call(ctx context.Context, id, method string, scope sdk.Scope, params any,
	consume func(Event) error,
) (json.RawMessage, error) {
	if lease := inboundLeaseFrom(ctx); lease != nil && lease.Manifest().ID == id {
		if lease.manager != m {
			return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension host changed during the native request")}
		}
		return lease.Call(ctx, method, scope, params, consume)
	}
	manifest, err := m.Manifest(id)
	if err != nil {
		return nil, err
	}
	if scope.Plugin == "" {
		scope.Plugin = id
	}
	if err := validateMethod(manifest, method, scope); err != nil {
		return nil, err
	}
	if _, err := m.authFor(scope); err != nil {
		return nil, err
	}
	lease, err := m.Acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	return lease.Call(ctx, method, scope, params, consume)
}

func (l *Lease) Call(ctx context.Context, method string, scope sdk.Scope, params any,
	consume func(Event) error,
) (json.RawMessage, error) {
	if l.closed.Load() {
		return nil, errors.New("extension request lease is closed")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	defer func() { stop(); cancel() }()
	m, manifest, client := l.manager, l.worker.entry.Manifest, l.client
	id := manifest.ID
	if scope.Plugin != "" && scope.Plugin != id {
		return nil, errors.New("extension scope belongs to another module")
	}
	scope.Plugin = id
	if err := validateMethod(manifest, method, scope); err != nil {
		return nil, err
	}
	auth, err := m.authFor(scope)
	if err != nil {
		return nil, err
	}
	effect := method == sdk.MethodProviderExecute || method == sdk.MethodToolCall
	component := sdk.PrimaryComponent
	if method == sdk.MethodModuleCall {
		encoded, marshalErr := json.Marshal(params)
		var request sdk.ModuleCallRequest
		if marshalErr != nil || !decodeHostParams(encoded, &request) || !json.Valid(request.Input) {
			return nil, errors.New("module.call requires a declared action and JSON input")
		}
		action, exists := manifest.ResolveAction(request.Action)
		if !exists || l.group == nil {
			return nil, errors.New("module action has no owned component group")
		}
		var err error
		client, err = l.group.actionClient(ctx, action)
		if err != nil {
			return nil, err
		}
		if action.Component != "" {
			component = action.Component
		}
		effect = action.Effect == "write"
	}
	// A lazy service may have needed time to start. Account replacement
	// during that wait revokes this generation before action dispatch.
	auth, err = m.authFor(scope)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, hostAuthorityKey{}, hostAuthority{manifest: manifest, method: method})
	if l.group != nil {
		effectName := "read"
		if effect {
			effectName = "write"
		}
		ctx = l.group.withAuthority(ctx, component, effectName)
	}
	if effect {
		if scope.Operation == "" {
			scope.Operation, err = randomID()
			if err != nil {
				return nil, err
			}
		}
		if scope.Attempt == 0 {
			scope.Attempt = 1
		}
		if _, err := m.store.Journal(scope, "dispatched"); err != nil {
			return nil, err
		}
	}
	accepted := false
	onEvent := func(event Event) error {
		if _, err := m.authFor(scope); err != nil {
			return err
		}
		if len(event.Data) > sdk.MaxChunkBytes*2 {
			return errors.New("extension event exceeds its contract")
		}
		if method == sdk.MethodModuleCall && event.Name != "accepted" {
			return errors.New("module RPC 1.1 is unary")
		}
		if event.Name == "accepted" {
			var value sdk.AcceptedEvent
			if !effect || json.Unmarshal(event.Data, &value) != nil || value.OperationID != scope.Operation {
				return errors.New("extension accepted an unknown operation")
			}
			if _, err := m.store.Journal(scope, "accepted"); err != nil {
				return err
			}
			accepted = true
		}
		if consume != nil {
			return consume(event)
		}
		return nil
	}
	result, err := client.call(ctx, method, scope, auth, params, onEvent)
	if effect {
		state := "completed"
		var declared struct {
			Outcome string `json:"outcome"`
		}
		if method != sdk.MethodModuleCall && err == nil && json.Unmarshal(result, &declared) == nil {
			switch declared.Outcome {
			case "accepted", "uncertain":
				err = &OutcomeError{Outcome: "uncertain", Cause: errors.New("extension left an operation unfinished")}
			case "not-started":
				if accepted {
					err = &OutcomeError{Outcome: "uncertain", Cause: errors.New("extension contradicted its accepted operation")}
				} else {
					state = "not-started"
				}
			}
		}
		if err != nil {
			state = "uncertain"
			var outcome *OutcomeError
			var rpc *sdk.Error
			if !accepted && (errors.As(err, &outcome) && outcome.Outcome == "not-started" ||
				errors.As(err, &rpc) && rpc.Outcome == "not-started") {
				state = "not-started"
			}
		}
		if _, stored := m.store.Journal(scope, state); stored != nil {
			return nil, &OutcomeError{Outcome: "uncertain", Cause: stored}
		}
		if err != nil && state == "uncertain" {
			return nil, &OutcomeError{Outcome: state, Cause: err}
		}
	}
	return result, err
}

func validateMethod(manifest sdk.Manifest, method string, scope sdk.Scope) error {
	if scope.Plugin != manifest.ID {
		return errors.New("extension scope belongs to another module")
	}
	if method != sdk.MethodDescribe {
		capability, known := sdk.MethodCapability(method)
		if !known || !manifest.Contract.Has(capability) || !slices.Contains(sdk.RequiredMethods(manifest), method) {
			return errors.New("extension method is outside its installed contract")
		}
	}
	return validateCallScope(manifest, method, scope)
}

func (m *Manager) Invoke(ctx context.Context, id, method string, scope sdk.Scope, params, out any,
	consume func(Event) error,
) error {
	result, err := m.Call(ctx, id, method, scope, params, consume)
	if err == nil && out != nil {
		err = json.Unmarshal(result, out)
	}
	return err
}

func validateCallScope(manifest sdk.Manifest, method string, scope sdk.Scope) error {
	needsProvider := method == sdk.MethodProviderExecute || method == sdk.MethodProviderModels ||
		method == sdk.MethodProviderUsage || method == sdk.MethodAuthStart || method == sdk.MethodAuthFinish ||
		method == sdk.MethodAuthRefresh || method == sdk.MethodAuthLogout
	if needsProvider || scope.Provider != "" {
		if !slices.ContainsFunc(manifest.Providers, func(provider sdk.ProviderDescriptor) bool {
			return provider.ID == scope.Provider
		}) {
			return errors.New("extension provider was not declared")
		}
	}
	if scope.Account == "" && scope.Generation != 0 || scope.Account != "" && scope.Generation == 0 {
		return errors.New("extension account generation is required")
	}
	if scope.Account != "" && !manifest.Contract.Has(sdk.AccountAuth) {
		return errors.New("extension did not negotiate account credential access")
	}
	if manifest.Contract.Has(sdk.AccountAuth) &&
		(method == sdk.MethodProviderExecute || method == sdk.MethodProviderUsage ||
			method == sdk.MethodAuthRefresh || method == sdk.MethodAuthLogout) && scope.Account == "" {
		return errors.New("extension provider needs a selected account")
	}
	return nil
}

func (m *Manager) authFor(scope sdk.Scope) (json.RawMessage, error) {
	if scope.Account == "" {
		return nil, nil
	}
	account, err := m.store.GetAccount(scope.Plugin, scope.Provider, scope.Account)
	if err != nil || account.Generation != scope.Generation {
		return nil, errors.New("extension account scope was revoked")
	}
	return account.Auth, nil
}

func (m *Manager) hostCall(ctx context.Context, scope sdk.Scope, method string, raw json.RawMessage) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	authority, authorized := ctx.Value(hostAuthorityKey{}).(hostAuthority)
	if !authorized || authority.manifest.ID != scope.Plugin {
		return nil, errors.New("host services are unavailable during module handshake")
	}
	if _, err := m.authFor(scope); err != nil {
		return nil, err
	}
	switch method {
	case sdk.MethodComponentCall:
		component, ok := ctx.Value(componentAuthorityKey{}).(componentAuthority)
		var request sdk.ComponentCallRequest
		if !ok || component.group == nil || !decodeHostParams(raw, &request) {
			return nil, errors.New("component call requires its parent module generation and declared binding")
		}
		auth, err := m.authFor(scope)
		if err != nil {
			return nil, err
		}
		return component.group.callBinding(ctx, component.component, request.Binding, scope, auth, request.Input)
	case sdk.MethodAccountReplace:
		if authority.method != sdk.MethodToolCall || !authority.manifest.Contract.Has(sdk.AccountAuth) ||
			scope.Account == "" || scope.Generation == 0 || scope.Operation == "" {
			return nil, errors.New("this call cannot replace account credentials")
		}
		operation, err := m.store.Operation(scope.Plugin, scope.Operation)
		if err != nil || operation.Scope != scope || operation.State != "accepted" {
			return nil, errors.New("account replacement requires a durable accepted operation")
		}
		var request sdk.AccountReplaceRequest
		if !decodeHostParams(raw, &request) || !json.Valid(request.Credentials) || string(request.Credentials) == "null" {
			return nil, errors.New("invalid replacement credentials")
		}
		account, err := m.store.SaveAccount(Account{
			Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: request.Credentials,
		}, scope.Generation)
		if err != nil {
			return nil, err
		}
		notifyChanged()
		return sdk.AccountReplaceResult{ID: account.ID, Generation: account.Generation}, nil
	case sdk.MethodNetworkProxy:
		var request sdk.NetworkProxyRequest
		if !decodeHostParams(raw, &request) || len(request.URL) > 8192 {
			return nil, errors.New("invalid network proxy request")
		}
		target, err := url.Parse(request.URL)
		if err != nil || target.Host == "" || target.User != nil {
			return nil, errors.New("invalid network proxy target")
		}
		switch target.Scheme {
		case "ws":
			target.Scheme = "http"
		case "wss":
			target.Scheme = "https"
		case "http", "https":
		default:
			return nil, errors.New("unsupported network proxy target")
		}
		networkRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, errors.New("invalid network proxy target")
		}
		proxy, err := netproxy.Func(networkRequest)
		if err != nil {
			return nil, errors.New("network proxy is unavailable")
		}
		result := sdk.NetworkProxyResult{}
		if proxy != nil {
			result.URL = proxy.String()
		}
		return result, nil
	case sdk.MethodStateGet:
		var request sdk.StateGetRequest
		if !decodeHostParams(raw, &request) {
			return nil, errors.New("invalid state request")
		}
		value, err := m.store.StateGet(scope, request.Key)
		if err != nil {
			return nil, err
		}
		return sdk.StateValue{
			Found: value.Version != 0, Version: strconv.FormatUint(value.Version, 10), Value: value.Value,
		}, nil
	case sdk.MethodStateCAS:
		var request sdk.StateCASRequest
		if !decodeHostParams(raw, &request) {
			return nil, errors.New("invalid state request")
		}
		if authority.manifest.Contract.Version.Minor >= 1 &&
			scope.Provider == "" && scope.Account == "" && request.Key == moduleConfigKey {
			return nil, errors.New("module configuration uses its dedicated CAS API")
		}
		var version uint64
		var err error
		if request.Version != "" {
			version, err = strconv.ParseUint(request.Version, 10, 64)
			if err != nil {
				return nil, errors.New("invalid state revision")
			}
		}
		value, err := m.store.StateCAS(scope, request.Key, version, request.Value)
		if err != nil {
			current, readErr := m.store.StateGet(scope, request.Key)
			if readErr == nil && current.Version != version {
				return sdk.StateCASResult{Swapped: false, Version: strconv.FormatUint(current.Version, 10)}, nil
			}
			return nil, err
		}
		return sdk.StateCASResult{Swapped: true, Version: strconv.FormatUint(value.Version, 10)}, nil
	default:
		return nil, errors.New("host method was not negotiated")
	}
}

func decodeHostParams(raw json.RawMessage, value any) bool {
	if len(raw) > sdk.MaxHostBytes {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value) == nil && decoder.Decode(new(any)) == io.EOF
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		done := m.closeDone
		err := m.closeErr
		m.mu.Unlock()
		if done == nil {
			return err
		}
		select {
		case <-done:
			return m.closeErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.closed = true
	if m.closeDone == nil {
		m.closeErr = ctx.Err()
		m.mu.Unlock()
		return m.closeErr
	}
	workers := m.workers
	m.workers = make(map[string]*managedWorker)
	m.mu.Unlock()
	m.stopStarts()
	for _, worker := range workers {
		worker.mu.Lock()
		worker.retiring = true
		idle, active := worker.idle, worker.active
		worker.mu.Unlock()
		if active > 0 {
			select {
			case <-idle:
			case <-ctx.Done():
			}
		}
		worker.mu.Lock()
		client := worker.client
		worker.mu.Unlock()
		if client != nil {
			_ = closeModuleClient(client)
		}
	}
	m.startWG.Wait()
	m.retireWG.Wait()
	m.closeErr = ctx.Err()
	close(m.closeDone)
	return m.closeErr
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("extension operation randomness unavailable: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}
