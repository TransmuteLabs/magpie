package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

const windowAuthRecordLimit = 64

type windowAuthRecord struct {
	Flow         string              `json:"flow"`
	Intent       string              `json:"intent"`
	Fingerprint  string              `json:"fingerprint"`
	Parent       string              `json:"parent"`
	Revision     string              `json:"revision"`
	Config       string              `json:"config"`
	Part         string              `json:"part"`
	Binding      string              `json:"binding"`
	Method       string              `json:"method"`
	Scope        sdk.Scope           `json:"scope"`
	Phase        string              `json:"phase"`
	Created      int64               `json:"created"`
	Expires      int64               `json:"expires"`
	Next         int64               `json:"next,omitempty"`
	Interval     int64               `json:"interval,omitempty"`
	Continuation json.RawMessage     `json:"continuation,omitempty"`
	URL          string              `json:"url,omitempty"`
	UserCode     string              `json:"userCode,omitempty"`
	Canceled     bool                `json:"canceled,omitempty"`
	Receipt      *sdk.AccountSummary `json:"receipt,omitempty"`
	Private      *sdk.AuthResult     `json:"private,omitempty"`
}

func (record windowAuthRecord) active() bool {
	switch record.Phase {
	case "reserved", "starting", "waiting", "polling":
		return true
	}
	return false
}

func (record *windowAuthRecord) UnmarshalJSON(raw []byte) error {
	probe := json.NewDecoder(bytes.NewReader(raw))
	probe.UseNumber()
	if catalogJSONValue(probe, 0) != nil || probe.Decode(new(any)) != io.EOF {
		return storeUnreadable()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return storeUnreadable()
	}
	for _, name := range []string{"flow", "intent", "fingerprint", "parent", "revision", "config", "part", "binding", "method", "scope", "phase", "created", "expires"} {
		if len(fields[name]) == 0 {
			return storeUnreadable()
		}
	}
	for name, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return storeUnreadable()
		}
		if name == "scope" || name == "receipt" || name == "private" {
			var nested map[string]json.RawMessage
			if json.Unmarshal(value, &nested) != nil || nested == nil {
				return storeUnreadable()
			}
			for _, item := range nested {
				if bytes.Equal(bytes.TrimSpace(item), []byte("null")) {
					return storeUnreadable()
				}
			}
		}
	}
	type plain windowAuthRecord
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return storeUnreadable()
	}
	*record = windowAuthRecord(value)
	return nil
}

func validWindowAuthRecords(records storeAccounts, plugin string) bool {
	if records.Schema < 4 {
		return records.AuthFlows == nil
	}
	if records.AuthFlows == nil || len(records.AuthFlows) > windowAuthRecordLimit {
		return false
	}
	flows, intents := map[string]bool{}, map[string]bool{}
	for _, record := range records.AuthFlows {
		if !storeToken(record.Flow) || len(record.Flow) != 64 || flows[record.Flow] ||
			!storeToken(record.Intent) || len(record.Intent) != 64 || intents[record.Intent] ||
			len(record.Fingerprint) != 64 || !storeToken(record.Fingerprint) ||
			record.Scope.Plugin != plugin || !storeValidScope(record.Scope) || record.Scope.Provider == "" ||
			record.Scope.Operation != "" || record.Scope.Session != "" || record.Scope.Attempt != 0 ||
			!storeToken(record.Parent) || !storeToken(record.Revision) || !storeToken(record.Config) ||
			record.Created <= 0 || record.Expires <= record.Created || record.Expires-record.Created > 600_000 ||
			(sdk.WindowAuthBindingDescriptor{ID: record.Binding, From: record.Part, Provider: record.Scope.Provider,
				Methods: []sdk.WindowAuthMethod{{ID: record.Method, Mode: "poll"}}}).Validate() != nil ||
			record.Next < 0 || record.Interval < 0 || record.Interval > 300_000 ||
			len(record.Continuation) > sdk.MaxHostBytes ||
			len(record.Continuation) != 0 && !json.Valid(record.Continuation) {
			return false
		}
		switch record.Phase {
		case "reserved", "starting", "waiting", "polling", "complete", "canceled", "expired", "uncertain":
		default:
			return false
		}
		if (record.Phase == "waiting" || record.Phase == "polling") &&
			(len(record.Continuation) == 0 || bytes.Equal(bytes.TrimSpace(record.Continuation), []byte("null")) ||
				record.Interval < 1000 || record.Next <= 0) {
			return false
		}
		if record.Phase == "complete" && record.Receipt == nil ||
			record.Phase != "complete" && record.Receipt != nil ||
			record.Private != nil && (record.Phase != "uncertain" || record.Private.Pending ||
				!storeToken(record.Private.ID) || record.Private.ExpiresAt < 0 ||
				len(record.Private.Credentials) == 0 || len(record.Private.Credentials) > sdk.MaxHostBytes ||
				!json.Valid(record.Private.Credentials) || bytes.Equal(bytes.TrimSpace(record.Private.Credentials), []byte("null"))) {
			return false
		}
		if record.public().Validate() != nil {
			return false
		}
		flows[record.Flow], intents[record.Intent] = true, true
	}
	return true
}

func (record windowAuthRecord) public() sdk.WindowAuthFlowView {
	view := sdk.WindowAuthFlowView{Flow: record.Flow, State: record.Phase}
	switch record.Phase {
	case "reserved", "starting", "polling":
		view.State = "pending"
	case "waiting":
		view.State, view.URL, view.UserCode, view.RetryAfterMs = "waiting", record.URL, record.UserCode, record.Interval
	case "complete":
		if record.Receipt != nil {
			receipt := *record.Receipt
			view.Account = &receipt
		}
	}
	if record.Canceled && view.State != "uncertain" {
		view = sdk.WindowAuthFlowView{Flow: record.Flow, State: "canceled"}
	}
	return view
}

func upgradeWindowAuthStore(records *storeAccounts) {
	if records.Schema < 3 {
		for index := range records.Accounts {
			records.Accounts[index].Usage = &usageState{}
		}
	}
	records.Schema = 4
	if records.AuthFlows == nil {
		records.AuthFlows = []windowAuthRecord{}
	}
}

func (s *Store) reserveWindowAuth(record windowAuthRecord) (windowAuthRecord, bool, error) {
	var result windowAuthRecord
	var created bool
	err := s.withStore(record.Scope.Plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, record.Scope.Plugin)
		if err != nil {
			return err
		}
		for _, previous := range records.AuthFlows {
			if previous.Intent == record.Intent {
				if previous.Fingerprint != record.Fingerprint || previous.Parent != record.Parent {
					return errors.New("window authentication intent changed")
				}
				result = previous
				return nil
			}
		}
		previous, found, err := readWindowAuthArchive(root, record.Scope.Plugin, "intent-"+record.Intent)
		if err != nil {
			return err
		}
		if found {
			if previous.Fingerprint != record.Fingerprint || previous.Parent != record.Parent {
				return errors.New("window authentication intent changed")
			}
			result = previous
			return nil
		}
		if err := archiveWindowAuthRecords(root, &records); err != nil {
			return err
		}
		if len(records.AuthFlows) >= windowAuthRecordLimit {
			return errors.New("window authentication history capacity is occupied")
		}
		active := 0
		for _, previous := range records.AuthFlows {
			if previous.Expires > time.Now().UnixMilli() && previous.active() {
				active++
			}
		}
		if active >= 16 {
			return errors.New("window authentication flow capacity is occupied")
		}
		if err := storeCheckWindowAuthConfig(root, record); err != nil {
			return err
		}
		if err := storeCheckGeneration(root, record.Scope); err != nil {
			return err
		}
		upgradeWindowAuthStore(&records)
		records.AuthFlows = append(records.AuthFlows, record)
		if !validWindowAuthRecords(records, record.Scope.Plugin) {
			return storeInvalid()
		}
		if err := storeWrite(root, "accounts.json", records, storeFileLimit); err != nil {
			return err
		}
		result, created = record, true
		return nil
	})
	return result, created, err
}

func storeCheckWindowAuthConfig(root *os.Root, record windowAuthRecord) error {
	value, err := storeReadState(root, sdk.Scope{Plugin: record.Scope.Plugin}, moduleConfigKey)
	if err != nil || strconv.FormatUint(value.Version, 10) != record.Config {
		return errors.New("window authentication configuration changed")
	}
	return nil
}

func (s *Store) windowAuthRecord(plugin, flow string) (windowAuthRecord, error) {
	var result windowAuthRecord
	if len(flow) != 64 || strings.Trim(flow, "0123456789abcdef") != "" {
		return result, storeInvalid()
	}
	err := s.withStore(plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, plugin)
		if err != nil {
			return err
		}
		for _, record := range records.AuthFlows {
			if record.Flow == flow {
				result = record
				return nil
			}
		}
		archived, found, err := readWindowAuthArchive(root, plugin, "flow-"+flow)
		if err != nil {
			return err
		}
		if found {
			result = archived
			return nil
		}
		return errors.New("window authentication flow is unavailable")
	})
	return result, err
}

func readWindowAuthArchive(root *os.Root, plugin, name string) (windowAuthRecord, bool, error) {
	var record windowAuthRecord
	directory, err := storeDirectory(root, "auth-receipts")
	if err != nil {
		return record, false, err
	}
	defer directory.Close()
	found, err := storeRead(directory, name+".json", storeFileLimit, &record)
	if err != nil || !found {
		return record, found, err
	}
	if record.Scope.Plugin != plugin || name != "flow-"+record.Flow && name != "intent-"+record.Intent ||
		!validWindowAuthRecords(storeAccounts{Schema: 4, AuthFlows: []windowAuthRecord{record}}, plugin) {
		return windowAuthRecord{}, false, storeUnreadable()
	}
	return record, true, nil
}

func archiveWindowAuthRecords(root *os.Root, records *storeAccounts) error {
	if len(records.AuthFlows) == 0 {
		return nil
	}
	directory, err := storeDirectory(root, "auth-receipts")
	if err != nil {
		return err
	}
	defer directory.Close()
	kept := make([]windowAuthRecord, 0, len(records.AuthFlows))
	for _, record := range records.AuthFlows {
		if record.Expires <= time.Now().UnixMilli() {
			switch record.Phase {
			case "starting", "polling":
				record.Phase = "uncertain"
			case "reserved", "waiting":
				record.Phase, record.Continuation = "expired", nil
			}
		}
		if record.Phase == "reserved" || record.Phase == "starting" || record.Phase == "waiting" || record.Phase == "polling" {
			kept = append(kept, record)
			continue
		}
		record.URL, record.UserCode = "", ""
		if err := storeWrite(directory, "intent-"+record.Intent+".json", record, storeFileLimit); err != nil {
			return err
		}
		if err := storeWrite(directory, "flow-"+record.Flow+".json", record, storeFileLimit); err != nil {
			return err
		}
	}
	records.AuthFlows = kept
	return nil
}

func (s *Store) updateWindowAuth(plugin, flow string, update func(*windowAuthRecord, *storeAccounts) error) (windowAuthRecord, error) {
	return s.updateWindowAuthThen(plugin, flow, update, nil)
}

func (s *Store) updateWindowAuthThen(plugin, flow string, update func(*windowAuthRecord, *storeAccounts) error, committed func(*os.Root)) (windowAuthRecord, error) {
	var result windowAuthRecord
	err := s.withStore(plugin, func(root *os.Root) error {
		records, err := storeReadAccounts(root, plugin)
		if err != nil {
			return err
		}
		for index := range records.AuthFlows {
			record := &records.AuthFlows[index]
			if record.Flow != flow {
				continue
			}
			if err := update(record, &records); err != nil {
				return err
			}
			if !validWindowAuthRecords(records, plugin) {
				return storeInvalid()
			}
			if err := storeWrite(root, "accounts.json", records, storeFileLimit); err != nil {
				return err
			}
			if committed != nil {
				committed(root)
			}
			result = *record
			return nil
		}
		return errors.New("window authentication flow is unavailable")
	})
	return result, err
}

func (s *Store) publishWindowAuth(plugin, flow string, result sdk.AuthResult) (windowAuthRecord, error) {
	var previous storeAccount
	var published Account
	return s.updateWindowAuthThen(plugin, flow, func(record *windowAuthRecord, records *storeAccounts) error {
		if record.Phase != "polling" || record.Canceled || time.Now().UnixMilli() >= record.Expires {
			return errors.New("window authentication authority ended before publication")
		}
		if result.ID == "" {
			result.ID = record.Scope.Account
		}
		if result.Pending || !storeToken(result.ID) || result.ExpiresAt < 0 ||
			result.ExpiresAt != 0 && result.ExpiresAt <= time.Now().UnixMilli() ||
			len(result.Credentials) == 0 || len(result.Credentials) > sdk.MaxHostBytes ||
			!json.Valid(result.Credentials) || bytes.Equal(bytes.TrimSpace(result.Credentials), []byte("null")) {
			return errors.New("window authentication returned an invalid account")
		}
		index := -1
		for position, account := range records.Accounts {
			if account.Provider == record.Scope.Provider && account.ID == result.ID {
				index = position
				break
			}
		}
		var generation uint64
		if record.Scope.Account != "" {
			if record.Scope.Account != result.ID || index < 0 || records.Accounts[index].Deleted ||
				records.Accounts[index].Generation != record.Scope.Generation {
				return errors.New("window authentication selected account changed")
			}
			if refresh := records.Accounts[index].Refresh; refresh != nil && refresh.Scope.Generation == record.Scope.Generation && refresh.State == "dispatched" {
				return errors.New("window authentication cannot replace a credential refresh in progress")
			}
			previous = records.Accounts[index]
			generation = record.Scope.Generation
		} else if index >= 0 {
			previous := records.Accounts[index]
			if !previous.Deleted {
				return errors.New("window authentication cannot replace an existing account")
			}
			generation = previous.Generation
		}
		if generation == ^uint64(0) {
			return errors.New("window authentication account generation is exhausted")
		}
		account := storeAccount{
			Account: Account{
				Plugin: plugin, Provider: record.Scope.Provider, ID: result.ID,
				Generation: generation + 1, Auth: bytes.Clone(result.Credentials), ExpiresAt: result.ExpiresAt,
			},
			Usage: &usageState{},
		}
		if index < 0 {
			records.Accounts = append(records.Accounts, account)
		} else {
			records.Accounts[index].Account = account.Account
			records.Accounts[index].Deleted = false
		}
		published = account.Account
		published.Name = result.Name
		record.Phase, record.Continuation, record.URL, record.UserCode = "complete", nil, "", ""
		record.Receipt = &sdk.AccountSummary{ID: result.ID, Generation: strconv.FormatUint(generation+1, 10), ExpiresAt: result.ExpiresAt}
		record.Interval, record.Next, record.Private = 0, 0, nil
		return nil
	}, func(root *os.Root) {
		if !storeToken(published.Name) && previous.Generation != 0 {
			published.Name = previous.namedAccount(root).Name
		}
		published.writeLabel(root)
	})
}
