package extension

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	sdk "github.com/yetone/magpie/pkg/extension"
)

const subscriptionOwnersFile = "subscription-owners.json"

type SubscriptionAccount struct {
	Key     string `json:"key"`
	ID      string `json:"id"`
	User    string `json:"user"`
	LoginID string `json:"loginID"`
}

type SubscriptionOwner struct {
	Module   string                `json:"module"`
	Provider string                `json:"provider"`
	PublicID string                `json:"publicID"`
	Revision string                `json:"revision"`
	Active   bool                  `json:"active"`
	Pending  bool                  `json:"pending,omitempty"`
	Accounts []SubscriptionAccount `json:"accounts"`
}

type SavedSubscriptionSource func(context.Context, string, func(map[string]json.RawMessage, []SubscriptionAccount, func() error) error) error

type subscriptionRecord struct {
	SubscriptionOwner
	GrantRevision string `json:"grantRevision"`

	Digests          map[string]string `json:"digests"`
	Tickets          []string          `json:"tickets"`
	Generations      []uint64          `json:"generations,omitempty"`
	AuthDispatched   bool              `json:"authDispatched,omitempty"`
	PreviousRevision string            `json:"previousRevision,omitempty"`
}

type subscriptionFlag uint8

const (
	subscriptionFlagAbsent subscriptionFlag = iota
	subscriptionFlagFalse
	subscriptionFlagTrue
)

func (flag *subscriptionFlag) UnmarshalJSON(raw []byte) error {
	if *flag != subscriptionFlagAbsent {
		return storeUnreadable()
	}
	switch string(raw) {
	case "false":
		*flag = subscriptionFlagFalse
	case "true":
		*flag = subscriptionFlagTrue
	default:
		return storeUnreadable()
	}
	return nil
}

var (
	errSubscriptionChanged = errors.New("the subscription selection changed or is incomplete")
	errRollbackUnverified  = errors.New("the transferred SDK credential state is no longer proven unchanged; automatic rollback is unavailable")
	errRollbackRefreshing  = errors.New("an SDK credential refresh is in progress or was interrupted; rollback requires a known outcome")
	errRollbackUncertain   = errors.New("an SDK credential refresh has an unknown outcome; automatic rollback is unavailable")
	errSDKSubscriptionBusy = errors.New("the SDK subscription is busy; try again when its current operations finish")
)

func validSubscriptionAccounts(accounts []SubscriptionAccount) bool {
	if len(accounts) == 0 || len(accounts) > savedSignInLimit {
		return false
	}
	keys, ids, users, logins := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, a := range accounts {
		for _, v := range []string{a.Key, a.ID, a.User, a.LoginID} {
			if !storeToken(v) || strings.TrimSpace(v) != v {
				return false
			}
		}
		id, user, login := strings.ToLower(a.ID), strings.ToLower(a.User), strings.ToLower(a.LoginID)
		if keys[a.Key] || ids[id] || users[user] || logins[login] {
			return false
		}
		keys[a.Key], ids[id], users[user], logins[login] = true, true, true, true
	}
	return true
}

func readSubscriptionRecords(dir *os.Root) ([]subscriptionRecord, error) {
	rows, _, err := storeLoadSnapshot(dir, subscriptionOwnersFile, savedSignInsDir, nil, decodeSubscriptionRecords)
	return rows, err
}

func decodeSubscriptionRecords(data []byte) ([]subscriptionRecord, error) {
	var stored []struct {
		subscriptionRecord
		Active         subscriptionFlag `json:"active"`
		Pending        subscriptionFlag `json:"pending"`
		AuthDispatched subscriptionFlag `json:"authDispatched"`
	}
	if err := storeDecode(data, &stored); err != nil {
		return nil, err
	}
	rows := make([]subscriptionRecord, 0, len(stored))
	public, pairs := map[string]bool{}, map[string]bool{}
	for _, row := range stored {
		if row.Active == subscriptionFlagAbsent {
			return nil, storeUnreadable()
		}
		r := row.subscriptionRecord
		r.Active = row.Active == subscriptionFlagTrue
		r.Pending = row.Pending == subscriptionFlagTrue
		r.AuthDispatched = row.AuthDispatched == subscriptionFlagTrue
		pair := r.Module + "/" + r.Provider
		if !storePluginID(r.Module) || !sdk.ValidID(r.Provider) || !sdk.ValidID(r.PublicID) || public[r.PublicID] || pairs[pair] ||
			len(r.GrantRevision) != 64 || !storeToken(r.Revision) || r.Pending && r.Active || !validSubscriptionAccounts(r.Accounts) || len(r.Digests) != len(r.Accounts) || len(r.Tickets) != len(r.Accounts) {
			return nil, storeUnreadable()
		}
		if len(r.Generations) != 0 && (len(r.Generations) != len(r.Accounts) || slices.Contains(r.Generations, 0)) {
			return nil, storeUnreadable()
		}
		for i, a := range r.Accounts {
			if len(r.Digests[a.Key]) != 64 || !storeToken(r.Tickets[i]) || i > 0 && r.Tickets[i] <= r.Tickets[i-1] {
				return nil, storeUnreadable()
			}
		}
		public[r.PublicID], pairs[pair] = true, true
		rows = append(rows, r)
	}
	return rows, nil
}

// SubscriptionOwners reads the ownership snapshot without creating the store.
// A pending reservation already hides its SDK pair.
func (s *Store) SubscriptionOwners(ctx context.Context) ([]SubscriptionOwner, error) {
	var out []SubscriptionOwner
	err := s.withReadStoreContext(ctx, savedSignInsDir, func(root *os.Root) error {
		var err error
		out, err = s.readSubscriptionOwners(root)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) readSubscriptionOwners(root *os.Root) ([]SubscriptionOwner, error) {
	if root == nil {
		return nil, nil
	}
	rows, _, err := storeLoadSnapshot(root, subscriptionOwnersFile, savedSignInsDir, &s.ownerSnapshot, decodeSubscriptionRecords)
	if err != nil {
		return nil, err
	}
	out := make([]SubscriptionOwner, 0, len(rows))
	for _, record := range rows {
		owner := record.SubscriptionOwner
		owner.Accounts = slices.Clone(owner.Accounts)
		out = append(out, owner)
	}
	return out, nil
}

func (r subscriptionRecord) checkSource(raw map[string]json.RawMessage, source []SubscriptionAccount) error {
	if len(raw) != len(r.Accounts) || len(source) != len(r.Accounts) {
		return errSubscriptionChanged
	}
	seen := map[string]bool{}
	for _, a := range source {
		i := slices.IndexFunc(r.Accounts, func(b SubscriptionAccount) bool { return a.Key == b.Key })
		if seen[a.Key] || i < 0 || r.Accounts[i].User != a.User || r.Accounts[i].LoginID != a.LoginID || len(raw[a.Key]) == 0 || savedSignInDigest(raw[a.Key]) != r.Digests[a.Key] {
			return errSubscriptionChanged
		}
		seen[a.Key] = true
	}
	return nil
}

func (r subscriptionRecord) hiddenAccount(candidate savedSignIn) int {
	if r.Active || r.Module != candidate.Module || r.Provider != candidate.Provider {
		return -1
	}
	return slices.IndexFunc(r.Accounts, func(account SubscriptionAccount) bool {
		return account.Key == candidate.Key && account.ID == candidate.Result.ID
	})
}

func (r subscriptionRecord) canReplaceSavedSignIn(candidate savedSignIn, current storeAccount) bool {
	index := r.hiddenAccount(candidate)
	if index < 0 || len(r.Generations) == 0 || current.Generation != candidate.Generation ||
		current.Generation == 0 || current.Generation == ^uint64(0) {
		return false
	}
	if r.Pending {
		return r.Digests[candidate.Key] == candidate.Digest && r.Generations[index] == current.Generation+1
	}
	return r.Generations[index] == current.Generation
}

func (m *Manager) ActivateSavedSubscription(ctx context.Context, module, provider, publicID string, tickets []string, source SavedSubscriptionSource) (SubscriptionOwner, error) {
	if !sdk.ValidID(publicID) || source == nil || len(tickets) == 0 || len(tickets) > savedSignInLimit {
		return SubscriptionOwner{}, errSubscriptionChanged
	}
	tickets = slices.Clone(tickets)
	slices.Sort(tickets)
	if len(slices.Compact(slices.Clone(tickets))) != len(tickets) {
		return SubscriptionOwner{}, errSubscriptionChanged
	}
	target, config, entry, err := m.savedSignInGrant(ctx, module, provider)
	if err != nil {
		return SubscriptionOwner{}, err
	}
	catalog, err := m.catalog.open(false)
	if err != nil {
		return SubscriptionOwner{}, err
	}
	defer catalog.Close()
	release, err := lockSubscriptionUse(ctx, catalog, module, provider, false)
	if err != nil {
		return SubscriptionOwner{}, err
	}
	defer release()
	var result SubscriptionOwner
	err = m.withWindowAuthAuthority(ctx, savedSignInAuthority(target, entry, config), func() error {
		return source(ctx, provider, func(raw map[string]json.RawMessage, accounts []SubscriptionAccount, checkSource func() error) error {
			return m.store.withPrivateStore(ctx, func(private *os.Root) error {
				dir, err := storeDirectory(private, savedSignInsDir)
				if err != nil {
					return err
				}
				defer dir.Close()
				root, err := storeDirectory(private, module)
				if err != nil {
					return err
				}
				defer root.Close()
				result, err = publishSubscription(ctx, dir, root, target, publicID, tickets, raw, accounts, checkSource)
				return err
			})
		})
	})
	if result.Revision != "" {
		notifyChanged()
	} else if err == nil {
		err = errSubscriptionChanged
	}
	return result, err
}

func publishSubscription(ctx context.Context, dir, root *os.Root, target ProviderSignIn, publicID string, tickets []string, raw map[string]json.RawMessage, accounts []SubscriptionAccount, checkSource func() error) (SubscriptionOwner, error) {
	var zero SubscriptionOwner
	rows, err := readSubscriptionRecords(dir)
	if err != nil {
		return zero, err
	}
	index := slices.IndexFunc(rows, func(r subscriptionRecord) bool {
		return r.PublicID == publicID || r.Module == target.Module && r.Provider == target.Provider
	})
	if index >= 0 {
		old := rows[index]
		if old.PublicID != publicID || old.Module != target.Module || old.Provider != target.Provider {
			return zero, errSubscriptionChanged
		}
		if old.Active {
			if old.GrantRevision != target.Revision || !slices.Equal(old.Tickets, tickets) || old.checkSource(raw, accounts) != nil {
				return zero, errSubscriptionChanged
			}
			current, err := storeReadAccounts(root, target.Module)
			if err != nil {
				return zero, err
			}
			for _, rec := range current.Accounts {
				if rec.Provider == target.Provider && !rec.Deleted && !slices.ContainsFunc(old.Accounts, func(a SubscriptionAccount) bool { return a.ID == rec.ID }) {
					return zero, errSubscriptionChanged
				}
			}
			for _, a := range old.Accounts {
				if !slices.ContainsFunc(current.Accounts, func(record storeAccount) bool {
					return !record.Deleted && record.Provider == target.Provider && record.ID == a.ID
				}) {
					return zero, errSubscriptionChanged
				}
			}
			if err := storeSync(dir); err != nil {
				return old.SubscriptionOwner, err
			}
			return old.SubscriptionOwner, consumeSubscriptionTickets(dir, tickets)
		}
		if !old.Pending && slices.Equal(old.Tickets, tickets) {
			return zero, errSubscriptionChanged
		}
	}
	saved, err := readSavedSignIns(dir)
	if err != nil {
		return zero, err
	}
	next := subscriptionRecord{
		SubscriptionOwner: SubscriptionOwner{
			Module: target.Module, Provider: target.Provider, PublicID: publicID,
			Accounts: slices.Clone(accounts), Pending: true,
		},
		GrantRevision: target.Revision, Tickets: tickets,
		Digests: make(map[string]string, len(accounts)), Generations: make([]uint64, len(accounts)),
	}
	var selected []savedSignIn
	for _, ticket := range tickets {
		i := slices.IndexFunc(saved, func(c savedSignIn) bool { return c.Ticket == ticket })
		if i < 0 {
			return zero, errSubscriptionChanged
		}
		c := saved[i]
		j := slices.IndexFunc(accounts, func(a SubscriptionAccount) bool { return a.Key == c.Key })
		if j < 0 || c.Module != target.Module || c.Provider != target.Provider || c.Revision != target.Revision || c.expired(time.Now().UnixMilli()) || next.Digests[c.Key] != "" {
			return zero, errSubscriptionChanged
		}
		if index >= 0 {
			old := rows[index]
			retry := old.Pending && slices.Equal(old.Tickets, tickets) && c.OwnerRevision == old.PreviousRevision
			if !retry && c.OwnerRevision != old.Revision {
				return zero, errSubscriptionChanged
			}
		} else if c.OwnerRevision != "" {
			return zero, errSubscriptionChanged
		}
		next.Accounts[j].ID = c.Result.ID
		next.Generations[j] = c.Generation + 1
		next.Digests[c.Key] = c.Digest
		selected = append(selected, c)
	}
	if len(tickets) != len(accounts) || !validSubscriptionAccounts(next.Accounts) || next.checkSource(raw, accounts) != nil {
		return zero, errSubscriptionChanged
	}
	records, err := storeReadAccounts(root, target.Module)
	if err != nil {
		return zero, err
	}
	for _, rec := range records.Accounts {
		if rec.Provider != target.Provider || rec.Deleted {
			continue
		}
		i := slices.IndexFunc(selected, func(c savedSignIn) bool { return c.Result.ID == rec.ID })
		if index < 0 || i < 0 {
			return zero, errSubscriptionChanged
		}
		candidate := selected[i]
		if rec.Generation == candidate.Generation+1 && candidate.matches(rec) {
			continue
		}
		if !rows[index].canReplaceSavedSignIn(candidate, rec) {
			return zero, errSubscriptionChanged
		}
	}
	for _, c := range selected {
		if rec, ok := savedSignInAccount(records, c); (!ok || rec.Deleted) && rec.Generation != c.Generation {
			return zero, errSubscriptionChanged
		}
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if index >= 0 && rows[index].Pending && slices.Equal(rows[index].Tickets, tickets) {
		next.PreviousRevision = rows[index].PreviousRevision
	} else if index >= 0 {
		next.PreviousRevision = rows[index].Revision
	}
	next.Revision, err = randomID()
	if err != nil {
		return zero, err
	}
	if index < 0 {
		index = len(rows)
		rows = append(rows, next)
	} else {
		rows[index] = next
	}
	check := func() error { return checkSubscriptionPublication(ctx, checkSource) }
	published, err := storePublishChecked(ctx, dir, subscriptionOwnersFile, rows, storeFileLimit, check)
	if err != nil {
		if published {
			return next.SubscriptionOwner, err
		}
		return zero, err
	}
	pending := next.SubscriptionOwner
	for _, c := range selected {
		if _, err := activateSavedSignIn(ctx, root, c, true); err != nil {
			return pending, err
		}
	}
	for _, c := range selected {
		if c.expired(time.Now().UnixMilli()) {
			return pending, errSubscriptionChanged
		}
	}
	if err := ctx.Err(); err != nil {
		return pending, err
	}
	next.Revision, err = randomID()
	if err != nil {
		return pending, err
	}
	next.Pending = false
	next.Active = true
	rows[index] = next
	published, err = storePublishChecked(ctx, dir, subscriptionOwnersFile, rows, storeFileLimit, check)
	if err != nil {
		if published {
			return next.SubscriptionOwner, err
		}
		return pending, err
	}
	return next.SubscriptionOwner, consumeSubscriptionTickets(dir, tickets)
}

func checkSubscriptionPublication(ctx context.Context, checkSource func() error) error {
	if checkSource != nil {
		if err := checkSource(); err != nil {
			return err
		}
	}
	return CheckSavedSignInDiscovery(ctx)
}

func consumeSubscriptionTickets(dir *os.Root, tickets []string) error {
	saved, err := readSavedSignIns(dir)
	if err != nil {
		return err
	}
	count := len(saved)
	saved = slices.DeleteFunc(saved, func(c savedSignIn) bool { return slices.Contains(tickets, c.Ticket) })
	if count == len(saved) {
		return nil
	}
	return writeSavedSignIns(dir, saved)
}

func (r subscriptionRecord) checkRollback(private *os.Root) error {
	if len(r.Generations) == 0 || r.AuthDispatched {
		return errRollbackUnverified
	}
	root, err := storeDirectory(private, r.Module)
	if err != nil {
		return err
	}
	defer root.Close()
	records, err := storeReadAccounts(root, r.Module)
	if err != nil {
		return err
	}
	remaining := len(r.Generations)
	for _, record := range records.Accounts {
		if record.Provider != r.Provider || record.Deleted {
			continue
		}
		index := slices.IndexFunc(r.Accounts, func(account SubscriptionAccount) bool { return account.ID == record.ID })
		if index < 0 || record.Generation != r.Generations[index] {
			return errRollbackUnverified
		}
		if refresh := record.Refresh; refresh != nil && refresh.Scope.Generation == record.Generation {
			switch refresh.State {
			case "dispatched":
				return errRollbackRefreshing
			case "uncertain":
				return errRollbackUncertain
			}
		}
		remaining--
	}
	if remaining != 0 {
		return errRollbackUnverified
	}
	return nil
}

func (m *Manager) RollbackSavedSubscription(ctx context.Context, publicID, expectedRevision string, source SavedSubscriptionSource) (SubscriptionOwner, error) {
	var result SubscriptionOwner
	if !sdk.ValidID(publicID) || !storeToken(expectedRevision) || source == nil {
		return result, errSubscriptionChanged
	}
	// Rollback must also work after the SDK has been disabled or removed.
	catalog, err := m.catalog.open(false)
	if err != nil {
		return result, err
	}
	defer catalog.Close()
	unlock, err := catalogLock(ctx, catalog)
	if err != nil {
		return result, err
	}
	err = func() error {
		defer unlock()
		var selected subscriptionRecord
		if err := m.store.withReadStoreContext(ctx, savedSignInsDir, func(dir *os.Root) error {
			rows, index, err := subscriptionRollbackSelection(dir, publicID, expectedRevision)
			if err != nil {
				return err
			}
			selected = rows[index]
			return nil
		}); err != nil {
			return err
		}
		publish := func(raw map[string]json.RawMessage, accounts []SubscriptionAccount, checkSource func() error) error {
			return m.store.withPrivateStore(ctx, func(private *os.Root) error {
				dir, err := storeOpenDirectory(private, savedSignInsDir, false)
				if err != nil {
					return err
				}
				defer dir.Close()
				rows, index, err := subscriptionRollbackSelection(dir, publicID, expectedRevision)
				if err != nil {
					return err
				}
				r := &rows[index]
				if r.Module != selected.Module || r.Provider != selected.Provider || r.Revision != selected.Revision {
					return errSubscriptionChanged
				}
				if !r.Active && !r.Pending {
					result = r.SubscriptionOwner
					return storeSync(dir)
				}
				if r.Active {
					if err := r.checkRollback(private); err != nil {
						return err
					}
				}
				if err := r.checkSource(raw, accounts); err != nil {
					return err
				}
				r.PreviousRevision = r.Revision
				r.Revision, err = randomID()
				if err != nil {
					return err
				}
				r.Active = false
				r.Pending = false
				if err := ctx.Err(); err != nil {
					return err
				}
				published, err := storePublishChecked(ctx, dir, subscriptionOwnersFile, rows, storeFileLimit,
					func() error { return checkSubscriptionPublication(ctx, checkSource) })
				if published {
					result = r.SubscriptionOwner
				}
				return err
			})
		}
		if !selected.Active && !selected.Pending {
			return publish(nil, nil, nil)
		}
		release, err := lockSubscriptionUse(ctx, catalog, selected.Module, selected.Provider, false)
		if err != nil {
			return err
		}
		defer release()
		return source(ctx, selected.Provider, publish)
	}()
	if result.Revision != "" {
		notifyChanged()
	} else if err == nil {
		err = errSubscriptionChanged
	}
	return result, err
}

func subscriptionRollbackSelection(dir *os.Root, publicID, expectedRevision string) ([]subscriptionRecord, int, error) {
	if dir == nil {
		return nil, -1, errSubscriptionChanged
	}
	rows, err := readSubscriptionRecords(dir)
	if err != nil {
		return nil, -1, err
	}
	index := slices.IndexFunc(rows, func(row subscriptionRecord) bool {
		if row.PublicID != publicID {
			return false
		}
		if row.Active || row.Pending {
			return row.Revision == expectedRevision
		}
		return row.PreviousRevision == expectedRevision
	})
	if index < 0 {
		return nil, -1, errSubscriptionChanged
	}
	return rows, index, nil
}

func lockSubscriptionUse(ctx context.Context, root *os.Root, module, provider string, shared bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := storeNamedLock(root, "subscription-"+storeHash(module, provider)+".lock")
	if err != nil {
		return nil, err
	}
	try := filelock.TryLock
	if shared {
		try = filelock.TryShared
	}
	held, err := try(file)
	if err != nil || !held {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, errSDKSubscriptionBusy
	}
	return func() {
		filelock.Unlock(file)
		file.Close()
	}, nil
}

var unconfirmedNativeSubscriptionCalls sync.Map

func (m *Manager) subscriptionAdmission(ctx context.Context, manifest sdk.Manifest, method string, scope sdk.Scope, authMutation bool) workerAdmission {
	revision, _ := SubscriptionOwnerRevision(ctx)
	transferable := revision != "" || manifest.Contract.Has(sdk.ProviderSavedSignIn)
	if !authMutation && (!transferable || scope.Account == "" && method != sdk.MethodProviderExecute) {
		return nil
	}
	return func(ctx context.Context) (func(bool), error) {
		return m.admitSubscriptionCall(ctx, method, scope, authMutation)
	}
}

func (m *Manager) admitSubscriptionCall(ctx context.Context, method string, scope sdk.Scope, authMutation bool) (func(bool), error) {
	root, err := m.catalog.open(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	release, err := lockSubscriptionUse(ctx, root, scope.Plugin, scope.Provider, true)
	if err != nil {
		return nil, err
	}
	if err := m.store.checkSubscriptionView(ctx, scope); err != nil {
		release()
		return nil, err
	}
	if providerExecuteSelection(ctx, method) == nil {
		if _, err := m.authForContext(ctx, scope); err != nil {
			release()
			return nil, err
		}
	}
	if err := checkWindowAdmission(ctx); err != nil {
		release()
		return nil, err
	}
	if authMutation {
		dispatch, _ := ctx.Value(windowAuthDispatchKey{}).(windowAuthDispatch)
		if err := m.fenceSubscriptionRollback(ctx, scope, dispatch); err != nil {
			release()
			return nil, err
		}
	}
	key := [3]string{root.Name(), scope.Plugin, scope.Provider}
	return func(confirmed bool) {
		if !confirmed {
			if _, held := unconfirmedNativeSubscriptionCalls.LoadOrStore(key, release); !held {
				return
			}
		}
		release()
	}, nil
}

func (m *Manager) fenceSubscriptionRollback(ctx context.Context, scope sdk.Scope, dispatch windowAuthDispatch) error {
	revision, _ := SubscriptionOwnerRevision(ctx)
	if revision == "" {
		return m.store.checkSubscriptionView(ctx, scope)
	}
	return m.withSubscriptionCatalogScope(ctx, scope, func(catalog *os.Root) error {
		return m.store.withPrivateStore(ctx, func(private *os.Root) error {
			dir, err := storeDirectory(private, savedSignInsDir)
			if err != nil {
				return err
			}
			defer dir.Close()
			rows, err := readSubscriptionRecords(dir)
			if err != nil {
				return err
			}
			var check func() error
			if dispatch.record.Flow != "" {
				check = func() error { return m.checkWindowAuthDispatch(ctx, catalog, private, dispatch) }
			}
			for index := range rows {
				record := &rows[index]
				if record.Module != scope.Plugin || record.Provider != scope.Provider || record.Revision != revision || !record.Active {
					continue
				}
				if record.AuthDispatched {
					if err := storeSync(dir); err != nil || check == nil {
						return err
					}
					return check()
				}
				record.AuthDispatched = true
				if check == nil {
					return storeWrite(dir, subscriptionOwnersFile, rows, storeFileLimit)
				}
				_, err := storePublishChecked(ctx, dir, subscriptionOwnersFile, rows, storeFileLimit, check)
				return err
			}
			return errSubscriptionChanged
		})
	})
}

type subscriptionOwnerRevisionKey struct{}

// WithSubscriptionOwnerRevision binds a provider view to its owner at display
// time. An empty revision names an SDK pair with no cutover record.
func WithSubscriptionOwnerRevision(ctx context.Context, revision string) context.Context {
	return context.WithValue(ctx, subscriptionOwnerRevisionKey{}, revision)
}

func SubscriptionOwnerRevision(ctx context.Context) (string, bool) {
	revision, bound := ctx.Value(subscriptionOwnerRevisionKey{}).(string)
	return revision, bound
}

func subscriptionJournalRevision(ctx context.Context, base string) string {
	if rev, _ := SubscriptionOwnerRevision(ctx); rev != "" {
		return storeHash(base, "subscription-owner", rev)
	}
	return base
}

func (s *Store) checkSubscriptionView(ctx context.Context, scope sdk.Scope) error {
	expected, _ := SubscriptionOwnerRevision(ctx)
	rows, err := s.SubscriptionOwners(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Module != scope.Plugin || r.Provider != scope.Provider {
			continue
		}
		if !r.Active || expected != r.Revision {
			return errSubscriptionChanged
		}
		if scope.Account == "" || slices.ContainsFunc(r.Accounts, func(a SubscriptionAccount) bool { return a.ID == scope.Account }) {
			return nil
		}
		for _, a := range r.Accounts {
			if strings.EqualFold(a.ID, scope.Account) || strings.EqualFold(a.User, scope.Account) || strings.EqualFold(a.LoginID, scope.Account) {
				return errSubscriptionChanged
			}
		}
		return nil
	}
	if expected != "" {
		return errSubscriptionChanged
	}
	return nil
}

// Deleted SDK accounts retain their identity so an Add cannot reclaim their
// public routing or account policy under a differently cased ID.
func (s *Store) checkSubscriptionResult(ctx context.Context, scope sdk.Scope, id string) error {
	if revision, _ := SubscriptionOwnerRevision(ctx); revision == "" {
		return nil
	}
	scope.Account = id
	if err := s.checkSubscriptionView(ctx, scope); err != nil {
		return err
	}
	return s.withReadStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		rows, err := s.readAccounts(root, scope.Plugin)
		if err != nil {
			return err
		}
		for _, r := range rows.Accounts {
			if r.Provider == scope.Provider && r.ID != id && strings.EqualFold(r.ID, id) {
				return errSubscriptionChanged
			}
		}
		return nil
	})
}

// DeleteSubscriptionAccount serializes sign-out against a subscription owner transition.
func (m *Manager) DeleteSubscriptionAccount(ctx context.Context, scope sdk.Scope, revision string) error {
	return m.withSubscriptionScope(WithSubscriptionOwnerRevision(ctx, revision), scope, func() error {
		return m.store.DeleteAccount(scope.Plugin, scope.Provider, scope.Account, scope.Generation)
	})
}

// CheckSubscriptionScope verifies a selection immediately before dispatch. Use
// withSubscriptionScope for the separate publication after a module call.
func (m *Manager) CheckSubscriptionScope(ctx context.Context, scope sdk.Scope, revision string) error {
	return m.withSubscriptionScope(WithSubscriptionOwnerRevision(ctx, revision), scope, func() error { return nil })
}

func (m *Manager) SaveSubscriptionAccount(ctx context.Context, scope sdk.Scope, account Account) (AccountGeneration, error) {
	var committed AccountGeneration
	err := m.withSubscriptionScope(ctx, scope, func() error {
		if account.Plugin != scope.Plugin || account.Provider != scope.Provider ||
			scope.Account != "" && account.ID != scope.Account {
			return errSubscriptionChanged
		}
		if err := m.store.checkSubscriptionResult(ctx, scope, account.ID); err != nil {
			return err
		}
		saved, err := m.store.saveAccountContext(ctx, account, scope.Generation, func() error {
			return CheckSavedSignInDiscovery(ctx)
		})
		if err != nil {
			return err
		}
		committed = AccountGeneration{ID: saved.ID, Generation: saved.Generation}
		return nil
	})
	return committed, err
}

func (m *Manager) withSubscriptionScope(ctx context.Context, scope sdk.Scope, action func() error) error {
	return m.withSubscriptionCatalogScope(ctx, scope, func(*os.Root) error { return action() })
}

func (m *Manager) withSubscriptionCatalogScope(ctx context.Context, scope sdk.Scope, action func(*os.Root) error) error {
	root, err := m.catalog.open(false)
	if err != nil {
		return err
	}
	defer root.Close()
	unlock, err := catalogLock(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.store.checkSubscriptionView(ctx, scope); err != nil {
		return err
	}
	return action(root)
}
