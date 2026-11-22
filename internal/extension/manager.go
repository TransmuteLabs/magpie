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
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/settings"
	sdk "github.com/yetone/magpie/pkg/extension"
)

type managedWorker struct {
	mu         sync.Mutex
	entry      Installed
	client     *workerClient
	active     int
	refreshing int
	retiring   bool
	idle       chan struct{}
	starting   chan struct{}
	config     sdk.ConfigSnapshot
}

type Manager struct {
	mu                sync.Mutex
	reloadMu          sync.Mutex
	root              string
	catalog           *Catalog
	store             *Store
	configMemos       [8]atomic.Pointer[moduleConfigMemo]
	protocolMu        sync.Mutex
	protocols         *installedProtocols
	workers           map[string]*managedWorker
	closed            bool
	closeDone         chan struct{}
	closeErr          error
	startCtx          context.Context
	stopStarts        context.CancelFunc
	startWG           sync.WaitGroup
	retireWG          sync.WaitGroup
	gatewayRuns       map[*GatewayRun]struct{}
	refreshes         map[refreshKey]*refreshRun
	refreshWG         sync.WaitGroup
	windowAuthOwners  map[string]*windowAuthOwner
	windowAuthWG      sync.WaitGroup
	activationWG      sync.WaitGroup
	gatewayActivation *GatewayActivation
	accountTasks      *AccountTasks
	accountTasksWG    sync.WaitGroup
	diagnostics       map[string]diagnosticWindow
	diagnosticLog     func(moduleDiagnostic)
	catalogReader     CatalogProjectionReader
	processes         map[*ownedProcess]struct{}
	processWG         sync.WaitGroup
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
	store := NewStore(root)
	store.accountSyncEnabled = true
	return &Manager{
		root: root, catalog: NewCatalog(root), store: store,
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
	ended   bool
}

func EndDefault(ctx context.Context) error {
	defaults.Lock()
	defaults.ended = true
	manager := defaults.manager
	defaults.Unlock()
	if manager == nil {
		return nil
	}
	return manager.Close(ctx)
}

// Default follows appdir's already selected portable/installed configuration.
// Changing a test's home never reuses a worker from the previous home.
func Default() *Manager {
	config := appdir.Config()
	defaults.Lock()
	if !defaults.ended && defaults.config == config && defaults.manager != nil && !defaults.manager.isClosed() {
		selected := defaults.manager
		defaults.Unlock()
		return selected
	}
	defaults.Unlock()
	root := filepath.Join(canonicalConfigPath(config), "extensions")
	defaults.Lock()
	if defaults.ended {
		defaults.Unlock()
		selected := NewManager(root)
		_ = selected.Close(context.Background())
		return selected
	}
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

func (m *Manager) ListContext(ctx context.Context) ([]Installed, error) {
	return m.catalog.list(ctx)
}

func (m *Manager) Manifest(id string) (sdk.Manifest, error) {
	return m.manifestContext(context.Background(), id)
}

func (m *Manager) manifestContext(ctx context.Context, id string) (sdk.Manifest, error) {
	var manifest sdk.Manifest
	err := m.catalog.withView(ctx, func(view catalogView) error {
		entry, found := view.lookup(id)
		if !found || entry.Disabled {
			return errors.New("extension is not enabled")
		}
		manifest = entry.Manifest.Clone()
		return nil
	})
	return manifest, err
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
		config, err := m.readModuleConfigContext(ctx, checkedModuleID)
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
		config, err := m.runtimeModuleConfigContext(handshake, candidate)
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
	_, err := m.reconcile(ctx, false)
	return err
}

type moduleGeneration struct {
	entry  Installed
	config sdk.ConfigSnapshot
}

func (m *Manager) reconcile(ctx context.Context, activate bool) (map[string]moduleGeneration, error) {
	if err := filelock.LockMutex(ctx, &m.reloadMu); err != nil {
		return nil, err
	}
	current, stopping, err := m.reconcileLocked(ctx, activate)
	m.reloadMu.Unlock()
	if err != nil || len(stopping) == 0 {
		return current, err
	}
	wait, cancel := context.WithTimeout(ctx, componentCloseTimeout)
	defer cancel()
	for _, stopped := range stopping {
		select {
		case <-stopped:
		case <-wait.Done():
			return current, wait.Err()
		}
	}
	return current, ctx.Err()
}

func (m *Manager) reconcileLocked(ctx context.Context, activate bool) (map[string]moduleGeneration, []<-chan struct{}, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("extension manager is closed")
	}
	tracked := make(map[string]bool, len(m.workers)+len(m.windowAuthOwners)+len(m.processes))
	for id := range m.workers {
		tracked[id] = true
	}
	for _, owner := range m.windowAuthOwners {
		tracked[owner.record.Scope.Plugin] = true
	}
	for parent := range m.processes {
		tracked[parent.scope.Plugin] = true
	}
	m.mu.Unlock()

	current := make(map[string]moduleGeneration, len(tracked))
	err := m.catalog.withView(ctx, func(view catalogView) error {
		for _, entry := range view.entries {
			if !activeRoot(entry) || (!tracked[entry.Manifest.ID] && !(activate && entry.Manifest.ActivatesGateway())) {
				continue
			}
			if entry = view.enabledEntry(entry); !activeRoot(entry) {
				continue
			}
			config, err := m.runtimeModuleConfigContext(ctx, entry)
			if err == nil {
				current[entry.Manifest.ID] = moduleGeneration{entry, config}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return nil, nil, err
	}
	if err != nil {
		current = nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("extension manager is closed")
	}
	var retiring []*managedWorker
	for id, worker := range m.workers {
		generation, found := current[id]
		if tracked[id] && (!found || !worker.matches(generation.entry, generation.config)) {
			m.detachWorkerLocked(id, worker)
			retiring = append(retiring, worker)
		}
	}
	m.mu.Unlock()
	var stopping []<-chan struct{}
	for _, worker := range retiring {
		if stopped := m.retire(worker); stopped != nil && !activate {
			stopping = append(stopping, stopped)
		}
	}
	m.revokeWindowAuthOwners(tracked, current)
	m.revokeOwnedProcesses(tracked, current)
	if err == nil {
		err = ctx.Err()
	}
	return current, stopping, err
}

func (w *managedWorker) matches(entry Installed, config sdk.ConfigSnapshot) bool {
	return w.entry.Revision == entry.Revision && w.entry.EnableGeneration == entry.EnableGeneration &&
		w.config.Version == config.Version
}

func (m *Manager) detachWorkerLocked(id string, worker *managedWorker) {
	worker.mu.Lock()
	worker.retiring = true
	worker.mu.Unlock()
	delete(m.workers, id)
	m.retireWG.Add(1)
}

func (m *Manager) retire(worker *managedWorker) <-chan struct{} {
	worker.mu.Lock()
	worker.retiring = true
	idle, draining := worker.idle, worker.active != 0
	worker.mu.Unlock()
	var stopped chan struct{}
	if !draining {
		stopped = make(chan struct{})
	}
	go func() {
		defer m.retireWG.Done()
		if stopped != nil {
			defer close(stopped)
		}
		if draining {
			timer := time.NewTimer(30 * time.Second)
			defer timer.Stop()
			select {
			case <-idle:
			case <-timer.C:
				worker.mu.Lock()
				refreshing := worker.refreshing > 0
				worker.mu.Unlock()
				if refreshing {
					timer.Reset(refreshLifetime)
					select {
					case <-idle:
					case <-m.startCtx.Done():
					case <-timer.C:
					}
				}
			case <-m.startCtx.Done():
			}
		}
		worker.mu.Lock()
		client := worker.client
		worker.mu.Unlock()
		if client != nil {
			_ = closeModuleClient(client)
		}
	}()
	return stopped
}

type expectedGeneration struct {
	packageRevision  string
	configVersion    string
	enableGeneration *string
	exact            bool
}

func (expected *expectedGeneration) matches(entry Installed, config string) bool {
	return expected == nil ||
		(expected.packageRevision == entry.Revision || !expected.exact && expected.packageRevision == "") &&
			(expected.configVersion == config || !expected.exact && expected.configVersion == "") &&
			(expected.enableGeneration == nil || *expected.enableGeneration == entry.EnableGeneration)
}

type providerExecuteSelectionKey struct{}
type providerExecutePolicyKey struct{}

func WithProviderExecutePolicy(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, providerExecutePolicyKey{}, check)
}

func WithProviderExecuteSelection(ctx context.Context, revision, config, epoch string) context.Context {
	return context.WithValue(ctx, providerExecuteSelectionKey{}, &expectedGeneration{
		packageRevision: revision, configVersion: config, enableGeneration: &epoch, exact: true,
	})
}

func providerExecuteSelection(ctx context.Context, method string) *expectedGeneration {
	if method != sdk.MethodProviderExecute {
		return nil
	}
	selected, _ := ctx.Value(providerExecuteSelectionKey{}).(*expectedGeneration)
	return selected
}

func (m *Manager) acquire(ctx context.Context, id string, expected *expectedGeneration) (*managedWorker, *workerClient, error) {
	worker, err := m.selectGenerationWorker(ctx, id, expected)
	if err != nil {
		return nil, nil, err
	}
	return m.acquireWorker(ctx, worker)
}

func (m *Manager) selectGenerationWorker(ctx context.Context, id string, expected *expectedGeneration) (*managedWorker, error) {
	var worker *managedWorker
	var stale *managedWorker
	err := func() error {
		if err := filelock.LockMutex(ctx, &m.reloadMu); err != nil {
			return err
		}
		defer m.reloadMu.Unlock()
		return m.catalog.withView(ctx, func(view catalogView) error {
			generation, err := m.workerGeneration(ctx, view, id)
			var current *moduleGeneration
			if err == nil {
				current = &generation
				entry := generation.entry
				if !expected.matches(entry, generation.config.Version) {
					err = errors.New("module generation changed before acquisition")
				} else {
					err = sdk.CheckCompatibility(entry.Manifest)
				}
			}
			worker, stale, err = m.selectWorker(ctx, id, current, true, err)
			return err
		})
	}()
	if stale != nil {
		m.retire(stale)
	}
	return worker, err
}

func (m *Manager) workerGeneration(ctx context.Context, view catalogView, id string) (moduleGeneration, error) {
	entry, found := view.lookup(id)
	if !found || !activeRoot(entry) {
		return moduleGeneration{}, errors.New("extension is not enabled")
	}
	entry = view.enabledEntry(entry)
	if entry.observerEnablementErr != nil {
		return moduleGeneration{}, entry.observerEnablementErr
	}
	config, err := m.runtimeModuleConfigContext(ctx, entry)
	if err != nil {
		return moduleGeneration{}, err
	}
	return moduleGeneration{entry, config}, nil
}

func (m *Manager) acquireSelected(ctx context.Context, id string, selected *Installed, config sdk.ConfigSnapshot) (*managedWorker, *workerClient, error) {
	worker, _, err := m.selectWorker(ctx, id, &moduleGeneration{*selected, config}, false, nil)
	if err != nil {
		return nil, nil, err
	}
	return m.acquireWorker(ctx, worker)
}

func (m *Manager) selectWorker(ctx context.Context, id string, current *moduleGeneration, authoritative bool, admissionErr error) (*managedWorker, *managedWorker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, nil, errors.New("extension manager is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if errors.Is(admissionErr, context.Canceled) || errors.Is(admissionErr, context.DeadlineExceeded) {
		return nil, nil, admissionErr
	}
	worker := m.workers[id]
	var stale *managedWorker
	if worker != nil && (current == nil || !worker.matches(current.entry, current.config)) {
		if !authoritative {
			return nil, nil, errors.New("module generation changed before cached worker admission")
		}
		m.detachWorkerLocked(id, worker)
		stale, worker = worker, nil
	}
	if admissionErr != nil {
		return nil, stale, admissionErr
	}
	if worker == nil {
		m.startLifecycleLocked()
		worker = &managedWorker{entry: current.entry, config: cloneModuleConfig(current.config)}
		m.workers[id] = worker
	}
	return worker, stale, nil
}

func (m *Manager) acquireWorker(ctx context.Context, worker *managedWorker) (*managedWorker, *workerClient, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		// Refresh retains a worker while holding the manager lock. Keep the
		// same order for restart and register startup before Close can wait.
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, nil, errors.New("extension manager is closed")
		}
		worker.mu.Lock()
		if worker.retiring {
			worker.mu.Unlock()
			m.mu.Unlock()
			return nil, nil, errors.New("extension generation is draining")
		}
		if worker.client != nil && worker.client.alive() {
			if worker.active == 0 {
				worker.idle = make(chan struct{})
			}
			worker.active++
			client := worker.client
			worker.mu.Unlock()
			m.mu.Unlock()
			return worker, client, nil
		}
		if waiting := worker.starting; waiting != nil {
			worker.mu.Unlock()
			m.mu.Unlock()
			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		m.startWG.Add(1)
		worker.starting = make(chan struct{})
		started := worker.starting
		old := worker.client
		worker.client = nil
		worker.mu.Unlock()
		m.mu.Unlock()
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
		if err == nil {
			err = m.checkActiveGeneration(handshake, worker.entry, worker.config.Version)
			if err != nil {
				_ = closeModuleClient(client)
				client = nil
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
	return m.newLease(ctx, worker, client), nil
}

func (m *Manager) newLease(ctx context.Context, worker *managedWorker, client *workerClient) *Lease {
	lifetime, cancel := context.WithCancel(ctx)
	return &Lease{manager: m, worker: worker, client: client, group: client.group, ctx: lifetime, cancel: cancel}
}

// AcquireNative reserves a logical codec response as well as its process
// generation. Separate RPC credits leave room to encode a provider's output
// even when inference calls occupy every ordinary execution slot.
func (m *Manager) AcquireNative(ctx context.Context, id string) (*Lease, error) {
	return m.acquireNative(ctx, id, nil)
}

func (m *Manager) acquireNative(ctx context.Context, id string, expected *expectedGeneration) (*Lease, error) {
	lease, err := m.acquireLease(ctx, id, expected)
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
		l.closeOwnedProcesses()
		if l.native {
			<-l.client.nativeSlots
		}
		releaseWorker(l.worker)
	})
}

func (m *Manager) Call(ctx context.Context, id, method string, scope sdk.Scope, params any,
	consume func(Event) error,
) (json.RawMessage, error) {
	if lease := inboundLeaseFrom(ctx); lease != nil && lease.worker.entry.Manifest.ID == id {
		if lease.manager != m {
			return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("extension host changed during the native request")}
		}
		return lease.Call(ctx, method, scope, params, consume)
	}
	if scope.Plugin == "" {
		scope.Plugin = id
	}
	expected := providerExecuteSelection(ctx, method)
	if method == sdk.MethodProviderExecute {
		if err := checkProviderExecuteDiscovery(ctx, scope); err != nil {
			return nil, &OutcomeError{Outcome: "not-started", Cause: err}
		}
	}
	worker, err := m.selectGenerationWorker(ctx, id, expected)
	if err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	params, err = validateCall(worker.entry.Manifest, method, scope, params)
	if err != nil {
		return nil, err
	}
	if method == sdk.MethodAuthRefresh {
		if err := netproxy.Held(ctx); err != nil {
			return nil, err
		}
	} else if err := m.checkAccountScope(ctx, scope); err != nil {
		return nil, beforeProviderDispatch(method, err)
	}
	worker, client, err := m.acquireWorker(ctx, worker)
	if err != nil {
		return nil, &OutcomeError{Outcome: "not-started", Cause: err}
	}
	lease := m.newLease(ctx, worker, client)
	defer lease.Close()
	return lease.callSelected(ctx, method, scope, params, consume, nil, true)
}

func (l *Lease) Call(ctx context.Context, method string, scope sdk.Scope, params any,
	consume func(Event) error,
) (json.RawMessage, error) {
	return l.call(ctx, method, scope, params, consume, nil)
}

func (l *Lease) call(ctx context.Context, method string, scope sdk.Scope, params any,
	consume func(Event) error, prepareResult func(json.RawMessage),
) (json.RawMessage, error) {
	return l.callSelected(ctx, method, scope, params, consume, prepareResult, false)
}

func (l *Lease) callSelected(ctx context.Context, method string, scope sdk.Scope, params any,
	consume func(Event) error, prepareResult func(json.RawMessage), validated bool,
) (json.RawMessage, error) {
	if l.closed.Load() {
		return nil, errors.New("extension request lease is closed")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	defer func() { stop(); cancel() }()
	m, manifest, client := l.manager, l.worker.entry.Manifest, l.client
	id := manifest.ID
	if scope.Plugin == "" {
		scope.Plugin = id
	}
	if !validated {
		var err error
		params, err = validateCall(manifest, method, scope, params)
		if err != nil {
			return nil, err
		}
	}
	expected := providerExecuteSelection(ctx, method)
	if !expected.matches(l.worker.entry, l.ConfigVersion()) {
		return nil, &OutcomeError{Outcome: "not-started", Cause: errors.New("module generation changed before provider execution")}
	}
	if method == sdk.MethodProviderExecute && !validated {
		if err := checkProviderExecuteDiscovery(ctx, scope); err != nil {
			return nil, &OutcomeError{Outcome: "not-started", Cause: err}
		}
	}
	if method == sdk.MethodAuthStart {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		if err := admitSavedSignInStart(ctx, raw); err != nil {
			return nil, err
		}
		params = json.RawMessage(raw)
	}
	var budgetRequest sdk.BudgetPolicyRequest
	if sdk.IsBudgetMethod(method) {
		raw, marshalErr := json.Marshal(params)
		if marshalErr != nil {
			return nil, marshalErr
		}
		request, err := sdk.DecodeBudgetRequest(manifest, method, raw)
		if err != nil {
			return nil, err
		}
		budgetRequest = request
		params = json.RawMessage(raw)
	}
	if method == sdk.MethodRequestObserve {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		if _, err := sdk.DecodeRequestObservation(raw); err != nil {
			return nil, err
		}
		// Send the exact validated envelope even if the caller gave us a custom marshaler.
		params = json.RawMessage(raw)
	}
	var outsideRead *sdk.OutsideCallsRequest
	if method == sdk.MethodProviderCalls {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		request, err := sdk.DecodeOutsideCallsRequest(manifest, scope, raw)
		if err != nil {
			return nil, err
		}
		outsideRead = &request
	}
	ctx, finishStorage, err := beginStorageCall(ctx, l, method, scope, params)
	if err != nil {
		return nil, err
	}
	defer finishStorage()
	if method == sdk.MethodProviderExecute {
		raw, err := providerExecutionParams(ctx, manifest.Contract.Version.Minor, params)
		if err != nil {
			return nil, err
		}
		if err := sdk.ValidateExecuteOperation(manifest, scope, raw); err != nil {
			return nil, err
		}
		params = raw
	}
	taskCompleted := false
	if method == sdk.MethodProviderExecute {
		var finish func(bool)
		var err error
		ctx, finish, err = m.beginAccountTaskForeground(ctx, l, scope, params)
		if err != nil {
			return nil, beforeProviderDispatch(method, err)
		}
		defer func() { finish(taskCompleted) }()
	}
	if method == sdk.MethodAuthRefresh {
		result, err := m.refreshFromLease(ctx, l, scope)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}
	authMutation := (method == sdk.MethodAuthStart || method == sdk.MethodAuthFinish || method == sdk.MethodAuthLogout) &&
		ctx.Value(savedSignInStateKey{}) == nil
	if authMutation {
		if err := m.store.checkSubscriptionView(ctx, scope); err != nil {
			return nil, err
		}
	}
	if sdk.BusinessCancel(manifest, method) {
		if err := validateCancelIntent(params, scope); err != nil {
			return nil, err
		}
		if manifest.Contract.Has(sdk.AccountAuth) && (scope.Account == "" || scope.Provider == "") {
			return nil, errors.New("business cancellation requires its selected account")
		}
	}
	if method == sdk.MethodProviderExecute {
		if err := m.store.UsageReadyContext(ctx, scope); err != nil {
			return nil, beforeProviderDispatch(method, err)
		}
	}
	var usageRead *UsageStamp
	if method == sdk.MethodProviderUsage {
		var err error
		params, usageRead, err = l.prepareUsageRead(ctx, scope, params)
		if err != nil {
			return nil, err
		}
	}
	if method == sdk.MethodToolCall {
		var err error
		ctx, err = checkLeasedTool(ctx, l, params)
		if err != nil {
			return nil, err
		}
	}
	auth, err := m.authForContext(ctx, scope)
	if err != nil {
		return nil, beforeProviderDispatch(method, err)
	}
	effect := method == sdk.MethodProviderExecute || method == sdk.MethodToolCall ||
		method == sdk.MethodGatewayResponse || method == sdk.MethodGatewaySignIn || sdk.BusinessCancel(manifest, method)
	component := sdk.PrimaryComponent
	if method == sdk.MethodModuleCall {
		encoded, marshalErr := json.Marshal(params)
		var request sdk.ModuleCallRequest
		if marshalErr != nil || !decodeParamsLimit(encoded, &request, sdk.MaxFrameBytes-4096) || !json.Valid(request.Input) {
			return nil, errors.New("module.call requires a declared action and JSON input")
		}
		action, exists := manifest.ResolveAction(request.Action)
		if !exists || l.group == nil {
			return nil, errors.New("module action has no owned component group")
		}
		if processParent(ctx) == nil {
			if selected, result, err := l.callDeclaredProcess(ctx, scope, action, params, consume); selected {
				return result, err
			}
		}
		client, err = l.group.actionClient(ctx, action)
		if err != nil {
			return nil, err
		}
		// A lazy service can outlive the selected account during startup.
		auth, err = m.authForContext(ctx, scope)
		if err != nil {
			return nil, err
		}
		if action.Component != "" {
			component = action.Component
		}
		effect = action.Effect == "write"
	}
	if err := checkLeasedCheckin(ctx, l, method, scope, params); err != nil {
		return nil, err
	}
	accountTask, err := checkLeasedAccountTask(ctx, l, method, scope, params)
	if err != nil {
		return nil, err
	}
	middleware := sdk.IsMiddlewareMethod(method)
	if !middleware {
		if err := checkWindowAdmission(ctx); err != nil {
			return nil, err
		}
	}
	ctx = withOwnCallParent(ctx, method)
	if method == sdk.MethodGatewayRequest || method == sdk.MethodSessionSourceRead || sdk.IsStorageMethod(method) || method == sdk.MethodLedgerObserve || middleware {
		// Retain this restriction when a recall delegates a read action to
		// a private component, which has its own module.call authority.
		ctx = context.WithValue(ctx, gatewayReadOnlyKey{}, true)
	}
	if l.group != nil {
		effectName := "read"
		if effect {
			effectName = "write"
		}
		ctx = l.group.withAuthority(ctx, component, effectName)
	}
	if parent := processParent(ctx); parent != nil {
		if parent.lease != l || parent.scope != scope || !effect ||
			method != sdk.MethodModuleCall && method != sdk.MethodToolCall {
			return nil, errors.New("owned process requires its original selected write parent")
		}
		parent.mu.Lock()
		if parent.bound {
			parent.mu.Unlock()
			return nil, errors.New("owned process parent cannot be reused")
		}
		parent.bound, parent.component = true, component
		parent.mu.Unlock()
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
		if _, err := m.store.journal(ctx, scope, "dispatched", method == sdk.MethodProviderExecute || accountTask); err != nil {
			return nil, beforeProviderDispatch(method, err)
		}
	}
	invocation := &invocationAuthority{scope: scope}
	ctx = context.WithValue(ctx, hostAuthorityKey{}, hostAuthority{manifest: manifest, method: method, invocation: invocation})
	ctx = beginRequestPolicy(ctx, l, scope, method, params)
	accepted := false
	onEvent := func(event Event) error {
		if method == sdk.MethodSessionSourceRead || sdk.IsStorageMethod(method) || method == sdk.MethodLedgerObserve || middleware {
			return errors.New("session source reads are unary")
		}
		if sdk.IsBudgetMethod(method) {
			return errors.New("budget policy is unary")
		}
		if method == sdk.MethodRequestObserve {
			return errors.New("request observation is unary")
		}
		if method == sdk.MethodProviderCalls {
			return errors.New("outside-call history reads are unary")
		}
		if err := m.checkAccountScope(ctx, scope); err != nil {
			return err
		}
		if err := checkWindowAdmission(ctx); err != nil {
			return err
		}
		if len(event.Data) > sdk.MaxChunkBytes*2 {
			return errors.New("extension event exceeds its contract")
		}
		if method == sdk.MethodModuleCall && event.Name != "accepted" &&
			!(accountTask && event.Name == sdk.EventAccountTaskUsage) {
			return errors.New("module RPC 1.1 is unary")
		}
		if event.Name == sdk.EventAccountTaskUsage {
			if !accountTask || !accepted {
				return errors.New("paid usage snapshots require their accepted scheduled task")
			}
			return m.captureAccountTaskUsage(ctx, scope, manifest, event.Data)
		}
		if sdk.IsGatewayMethod(method) && event.Name != "accepted" {
			return errors.New("gateway hook calls are unary")
		}
		if sdk.BusinessCancel(manifest, method) && event.Name != "accepted" {
			return errors.New("business cancellation is unary")
		}
		if event.Name == "accepted" {
			var value sdk.AcceptedEvent
			if !effect || json.Unmarshal(event.Data, &value) != nil || value.OperationID != scope.Operation {
				return errors.New("extension accepted an unknown operation")
			}
			if method == sdk.MethodToolCall && scope.Account != "" && manifest.Contract.Has(sdk.AccountAuth) {
				if err := m.fenceSubscriptionRollback(ctx, scope); err != nil {
					return err
				}
			}
			if _, err := m.store.Journal(scope, "accepted"); err != nil {
				return err
			}
			accepted = true
			if parent := processParent(ctx); parent != nil {
				parent.observe("accepted")
			}
			if accountTask {
				if _, err := m.store.InvalidateUsage(scope); err != nil {
					return err
				}
			}
		}
		observeRequestOutput(ctx, event)
		if consume != nil {
			return consume(event)
		}
		return nil
	}
	if method == sdk.MethodProviderExecute {
		if record, ok := ctx.Value(providerExecutionBindingKey{}).(func(ProviderExecutionBinding)); ok && record != nil {
			record(ProviderExecutionBinding{
				Namespace: l.Namespace(), PackageRevision: l.Revision(), ConfigVersion: l.ConfigVersion(),
				EnableGeneration: l.worker.entry.EnableGeneration, Scope: scope,
			})
		}
	}
	admit := m.subscriptionAdmission(ctx, manifest, method, scope, authMutation)
	if method == sdk.MethodProviderExecute && (expected != nil || scope.Account != "") {
		admit = l.providerExecuteAdmission(scope, expected != nil, admit)
	}
	if effect && scope.Provider != "" && manifest.Contract.Version.Minor >= 8 && manifest.Contract.Has(sdk.UsageReporter) &&
		(method == sdk.MethodModuleCall || method == sdk.MethodToolCall) {
		admit = m.ownCallAdmission(ctx, scope, admit)
	}
	if middleware {
		previous := admit
		admit = func(dispatch context.Context) (func(bool), error) {
			if err := checkWindowAdmission(dispatch); err != nil {
				return nil, err
			}
			if previous != nil {
				return previous(dispatch)
			}
			return nil, nil
		}
	}
	result, err := client.callWithAdmission(ctx, method, scope, auth, params, onEvent, admit)
	if err == nil {
		if prepareResult != nil {
			prepareResult(result)
		}
		err = m.checkResultPublication(ctx, invocation)
		if err != nil {
			result = nil
		}
	}
	if parent := processParent(ctx); parent != nil {
		err = parent.result(result, err)
	}
	if err == nil && accountTask {
		err = m.finishAccountTaskCall(ctx, scope, manifest, result, accepted)
	}
	if accountTask {
		if flushErr := m.flushAccountTaskUsage(ctx, scope, err); err == nil {
			err = flushErr
		}
	}
	if err == nil && sdk.IsBudgetMethod(method) {
		_, err = sdk.DecodeBudgetResult(method, budgetRequest, result)
		if err != nil {
			result = nil
		}
	}
	if err == nil && (method == sdk.MethodRequestObserve || method == sdk.MethodLedgerObserve) {
		if method == sdk.MethodLedgerObserve {
			err = sdk.ValidateLedgerObservationResult(result)
		} else {
			err = sdk.ValidateRequestObservationResult(result)
		}
		if err != nil {
			result = nil
		}
	}
	if err == nil && sdk.IsStorageMethod(method) {
		err = finishStorageCall(ctx, result)
	}
	if err == nil && method == sdk.MethodProviderUsage {
		err = l.finishUsageRead(ctx, scope, usageRead, result)
	}
	if err == nil && outsideRead != nil {
		_, err = sdk.DecodeOutsideCallsResult(manifest, *outsideRead, result)
		if err == nil {
			err = m.checkAccountScope(ctx, scope)
		}
	}
	if effect {
		state := "completed"
		var declared struct {
			Outcome string          `json:"outcome"`
			Refusal json.RawMessage `json:"refusal"`
		}
		if method != sdk.MethodModuleCall && !sdk.IsGatewayMethod(method) && err == nil && json.Unmarshal(result, &declared) == nil {
			if method == sdk.MethodProviderExecute && len(declared.Refusal) > 0 && !bytes.Equal(declared.Refusal, []byte("null")) {
				err = checkFinalExecutionRefusal(ctx, manifest, result)
			}
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
		if parent := processParent(ctx); parent != nil {
			parent.observe(state)
		}
		if err != nil && state == "uncertain" {
			return nil, &OutcomeError{Outcome: state, Cause: err}
		}
	}
	taskCompleted = err == nil
	return result, err
}

func (l *Lease) providerExecuteAdmission(scope sdk.Scope, exact bool, next workerAdmission) workerAdmission {
	return func(ctx context.Context) (func(bool), error) {
		if err := checkProviderExecuteDiscovery(ctx, scope); err != nil {
			return nil, err
		}
		m := l.manager
		var release func(bool)
		var err error
		if next != nil {
			release, err = next(ctx)
		}
		if err == nil {
			checkSelection := func() error {
				if exact || next == nil {
					if err := m.checkAccountScope(ctx, scope); err != nil {
						return err
					}
				}
				if err := checkProviderExecuteDiscovery(ctx, scope); err != nil {
					return err
				}
				if check, ok := ctx.Value(providerExecutePolicyKey{}).(func(context.Context) error); ok && check != nil {
					return check(ctx)
				}
				return nil
			}
			if exact {
				err = m.withActiveGeneration(ctx, l.worker.entry, l.ConfigVersion(), checkSelection)
			} else {
				err = checkSelection()
			}
		}
		if err != nil {
			if release != nil {
				release(true)
			}
			return nil, err
		}
		return release, nil
	}
}

func checkProviderExecuteDiscovery(ctx context.Context, scope sdk.Scope) error {
	if err := ctx.Err(); err != nil || scope.Account == "" {
		return err
	}
	off, err := settings.AccountDiscoveryOffNow()
	if off && err == nil {
		return storeError("account_discovery_off", "Subscription accounts are off in magpie.")
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

func beforeProviderDispatch(method string, err error) error {
	var refusal *sdk.Error
	if method == sdk.MethodProviderExecute && errors.As(err, &refusal) &&
		refusal.Outcome == "" && (refusal.Code == "stale_generation" || refusal.Code == "not_found") {
		return &OutcomeError{Outcome: "not-started", Cause: err}
	}
	return err
}

func validateCall(manifest sdk.Manifest, method string, scope sdk.Scope, params any) (any, error) {
	if err := validateMethod(manifest, method, scope); err != nil {
		return nil, err
	}
	if method == sdk.MethodSessionSourceRead || sdk.IsStorageMethod(method) {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		params = json.RawMessage(raw)
	}
	if err := validateSessionSourceScope(manifest, method, scope, params); err != nil {
		return nil, err
	}
	if err := validateStorageScope(manifest, method, scope, params); err != nil {
		return nil, err
	}
	return params, nil
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
	if sdk.IsBudgetMethod(method) {
		return sdk.ValidateBudgetScope(scope)
	}
	if method == sdk.MethodRequestObserve {
		return sdk.ValidateRequestObservationScope(scope)
	}
	if sdk.IsMiddlewareMethod(method) {
		if err := sdk.ValidateMiddlewareScope(scope); err != nil {
			return err
		}
	}
	if method == sdk.MethodLedgerObserve && (scope.Provider != "" || scope.Account != "" || scope.Generation != 0 ||
		scope.Session != "" || scope.Operation != "" || scope.Attempt != 0) {
		return errors.New("ledger observation cannot borrow account, session or write authority")
	}
	if err := validateGatewayScope(method, scope); err != nil {
		return err
	}
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
	if method == sdk.MethodProviderExecute || method == sdk.MethodProviderModels {
		if err := sdk.ValidateInferenceProvider(manifest, scope); err != nil {
			return err
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
	return m.authForContext(context.Background(), scope)
}

func (m *Manager) authForContext(ctx context.Context, scope sdk.Scope) (json.RawMessage, error) {
	return m.readAccountAuth(ctx, scope, true)
}

func (m *Manager) checkAccountScope(ctx context.Context, scope sdk.Scope) error {
	_, err := m.readAccountAuth(ctx, scope, false)
	return err
}

func (m *Manager) readAccountAuth(ctx context.Context, scope sdk.Scope, copyAuth bool) (json.RawMessage, error) {
	if scope.Account == "" {
		return nil, ctx.Err()
	}
	account, err := m.store.readAccountContext(ctx, scope.Plugin, scope.Provider, scope.Account, &scope.Generation, copyAuth)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		var refusal *sdk.Error
		if errors.As(err, &refusal) && (refusal.Code == "not_found" || refusal.Code == "stale_generation") {
			return nil, storeError(refusal.Code, "extension account scope was revoked")
		}
		return nil, errors.New("extension account scope was revoked")
	}
	return account.Auth, nil
}

func (m *Manager) hostCall(ctx context.Context, scope sdk.Scope, method string, raw json.RawMessage) (any, error) {
	authority, authorized := ctx.Value(hostAuthorityKey{}).(hostAuthority)
	if !authorized || authority.manifest.ID != scope.Plugin {
		return nil, errors.New("host services are unavailable during module handshake")
	}
	if method == sdk.MethodRequestRecover {
		return m.recoverRequest(ctx, scope, raw, authority)
	}
	if err := checkWindowAdmission(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.checkAccountScope(ctx, scope); err != nil {
		return nil, err
	}
	if sdk.IsBudgetMethod(authority.method) {
		return nil, errors.New("budget policy cannot call host services")
	}
	if authority.method == sdk.MethodRequestObserve {
		return nil, errors.New("request observation cannot call host services")
	}
	if sdk.IsProcessHostMethod(method) {
		return m.processHostCall(ctx, scope, method, raw, authority)
	}
	if sdk.IsStorageHostMethod(method) {
		return m.storageHostCall(ctx, scope, method, raw, authority)
	}
	if sdk.IsOwnCallHostMethod(method) {
		return m.appendOwnCall(ctx, scope, raw, authority)
	}
	if sdk.IsAccountTaskHostMethod(method) {
		return m.accountTaskHostCall(ctx, scope, method, raw, authority)
	}
	switch method {
	case sdk.MethodMemorySearch:
		return m.searchToolMemory(ctx, raw, authority)
	case sdk.MethodCatalogRead:
		return m.readCatalogProjection(ctx, scope, raw, authority)
	case sdk.MethodDiagnosticEmit:
		return m.emitDiagnostic(scope, raw, authority)
	case sdk.MethodUsageInvalidate:
		return m.invalidateUsage(ctx, scope, raw, authority)
	case sdk.MethodComponentCall:
		component, ok := ctx.Value(componentAuthorityKey{}).(componentAuthority)
		var request sdk.ComponentCallRequest
		if !ok || component.group == nil || !decodeHostParams(raw, &request) {
			return nil, errors.New("component call requires its parent module generation and declared binding")
		}
		auth, err := m.authForContext(ctx, scope)
		if err != nil {
			return nil, err
		}
		return component.group.callBinding(ctx, component.component, request.Binding, scope, auth, request.Input)
	case sdk.MethodAccountReplace:
		if authority.method != sdk.MethodToolCall || !authority.manifest.Contract.Has(sdk.AccountAuth) ||
			scope.Account == "" || scope.Generation == 0 || scope.Operation == "" ||
			authority.invocation == nil || authority.invocation.scope != scope {
			return nil, errors.New("this call cannot replace account credentials")
		}
		operation, err := m.store.OperationContext(ctx, scope.Plugin, scope.Operation)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || operation.Scope != scope || operation.State != "accepted" {
			return nil, errors.New("account replacement requires a durable accepted operation")
		}
		var request sdk.AccountReplaceRequest
		if !decodeHostParams(raw, &request) || !json.Valid(request.Credentials) || string(request.Credentials) == "null" {
			return nil, errors.New("invalid replacement credentials")
		}
		var account Account
		err = m.withSubscriptionScope(ctx, scope, func() error {
			var err error
			account, err = m.store.SaveAccount(Account{
				Plugin: scope.Plugin, Provider: scope.Provider, ID: scope.Account, Auth: request.Credentials,
			}, scope.Generation)
			return err
		})
		if err != nil {
			return nil, err
		}
		authority.invocation.replacement.Store(account.Generation)
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
		get := m.store.StateGetContext
		if temporary, ok := ctx.Value(savedSignInStateKey{}).(*savedSignInState); ok {
			get = temporary.get
		}
		value, err := get(ctx, scope, request.Key)
		if err != nil {
			return nil, err
		}
		return sdk.StateValue{
			Found: value.Version != 0, Version: strconv.FormatUint(value.Version, 10), Value: value.Value,
		}, nil
	case sdk.MethodStateCAS:
		if readOnly, _ := ctx.Value(gatewayReadOnlyKey{}).(bool); readOnly {
			return nil, errors.New("gateway recall cannot write persistent module state")
		}
		authControl := authority.manifest.Contract.Has(sdk.AccountAuth) &&
			(authority.method == sdk.MethodAuthStart || authority.method == sdk.MethodAuthFinish ||
				authority.method == sdk.MethodAuthLogout)
		if component, ok := ctx.Value(componentAuthorityKey{}).(componentAuthority); ok &&
			authority.manifest.Contract.Version.Minor >= 4 && component.effect != "write" && !authControl {
			return nil, errors.New("a read-only module call cannot write persistent state")
		}
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
		get, cas := m.store.StateGetContext, m.store.StateCASContext
		if temporary, ok := ctx.Value(savedSignInStateKey{}).(*savedSignInState); ok {
			get, cas = temporary.get, temporary.cas
		}
		value, err := cas(ctx, scope, request.Key, version, request.Value)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			current, readErr := get(ctx, scope, request.Key)
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
	return decodeParamsLimit(raw, value, sdk.MaxHostBytes)
}

func decodeParamsLimit(raw json.RawMessage, value any, limit int) bool {
	if len(raw) > limit {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value) == nil && decoder.Decode(new(any)) == io.EOF
}

func (m *Manager) Close(ctx context.Context) error {
	if m.store != nil {
		m.store.closeAccountSync()
	}
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
	runs := make([]*GatewayRun, 0, len(m.gatewayRuns))
	for run := range m.gatewayRuns {
		runs = append(runs, run)
	}
	m.workers = make(map[string]*managedWorker)
	m.mu.Unlock()
	go m.finishClose(workers, runs)
	select {
	case <-m.closeDone:
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) finishClose(workers map[string]*managedWorker, runs []*GatewayRun) {
	m.stopStarts()
	m.processWG.Wait()
	m.accountTasksWG.Wait()
	m.activationWG.Wait()
	for _, run := range runs {
		run.abort()
		<-run.done
	}
	m.refreshWG.Wait()
	m.windowAuthWG.Wait()
	for _, worker := range workers {
		worker.mu.Lock()
		worker.retiring = true
		client := worker.client
		worker.mu.Unlock()
		if client != nil {
			_ = closeModuleClient(client)
		}
	}
	m.startWG.Wait()
	m.retireWG.Wait()
	close(m.closeDone)
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("extension operation randomness unavailable: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}
