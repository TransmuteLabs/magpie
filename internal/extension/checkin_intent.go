package extension

import (
	"context"
	"errors"
	"os"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type CheckinExecution struct {
	Result  *sdk.AccountCheckinResult `json:"result,omitempty"`
	Receipt JournalEntry              `json:"receipt"`
}

type checkinIntent struct {
	Day     string                    `json:"day"`
	Receipt JournalEntry              `json:"receipt"`
	Result  *sdk.AccountCheckinResult `json:"result,omitempty"`
}

func checkinIntentName(scope sdk.Scope) string {
	return storeHash(scope.Provider+"\x00"+scope.Account) + ".json"
}

func readCheckinIntent(root *os.Root, scope sdk.Scope) (checkinIntent, bool, error) {
	var record checkinIntent
	found, err := storeRead(root, checkinIntentName(scope), storeFileLimit, &record)
	if err != nil || !found {
		return record, found, err
	}
	saved := record.Receipt.Scope
	if saved.Plugin != scope.Plugin || saved.Provider != scope.Provider || saved.Account != scope.Account ||
		!storeValidScope(saved) || !storeToken(saved.Operation) || saved.Attempt != 1 || saved.Session != "" ||
		(sdk.AccountCheckinRequest{Day: record.Day}).Validate() != nil || record.Receipt.Updated.IsZero() ||
		!storeJournalState(record.Receipt.State) && record.Receipt.State != "reserved" ||
		record.Result != nil && (record.Receipt.State != "completed" || record.Result.Validate() != nil) {
		return record, true, storeUnreadable()
	}
	return record, true, nil
}

func checkinExecution(record checkinIntent) CheckinExecution {
	result := CheckinExecution{Receipt: record.Receipt}
	if record.Result != nil {
		copy := record.Result.Clone()
		result.Result = &copy
	}
	return result
}

func (s *Store) reserveCheckin(ctx context.Context, scope sdk.Scope, day string) (checkinIntent, bool, error) {
	var selected checkinIntent
	dispatch := false
	if !storeValidScope(scope) || scope.Account == "" || scope.Operation != "" || scope.Session != "" ||
		scope.Attempt != 0 || (sdk.AccountCheckinRequest{Day: day}).Validate() != nil {
		return selected, false, storeInvalid()
	}
	err := s.withStoreContext(ctx, scope.Plugin, func(root *os.Root) error {
		if err := storeCheckGeneration(root, scope); err != nil {
			return err
		}
		dir, err := storeDirectory(root, "checkins")
		if err != nil {
			return err
		}
		defer dir.Close()
		previous, found, err := readCheckinIntent(dir, scope)
		if err != nil {
			return err
		}
		if found && previous.Receipt.State != "not-started" {
			if previous.Result == nil || previous.Result.Outcome == "failed" {
				selected = previous
				return &OutcomeError{Outcome: "uncertain", Cause: errors.New("check-in has an unresolved write; reconcile its operation before retrying")}
			}
			until, _ := time.Parse(time.RFC3339, previous.Result.Until)
			if previous.Result.Outcome == sdk.AccountCheckinRateLimited && time.Now().Before(until) {
				selected = previous
				return nil
			}
			switch previous.Result.Outcome {
			case "claimed", "captcha", "own-app":
				if !until.IsZero() && time.Now().Before(until) || until.IsZero() && previous.Day == day {
					selected = previous
					return nil
				}
			}
		}
		operation, err := randomID()
		if err != nil {
			return err
		}
		scope.Operation, scope.Attempt = operation, 1
		record := checkinIntent{Day: day, Receipt: JournalEntry{Scope: scope, State: "reserved", Updated: time.Now().UTC()}}
		published, err := storePublishContext(ctx, dir, checkinIntentName(scope), record, storeFileLimit)
		if published {
			selected = record
		}
		if err != nil {
			return err
		}
		dispatch = true
		return nil
	})
	return selected, dispatch, err
}

func (s *Store) finishCheckin(record checkinIntent) error {
	return s.withStore(record.Receipt.Scope.Plugin, func(root *os.Root) error {
		dir, err := storeDirectory(root, "checkins")
		if err != nil {
			return err
		}
		defer dir.Close()
		current, found, err := readCheckinIntent(dir, record.Receipt.Scope)
		if err != nil {
			return err
		}
		if !found || current.Receipt.Scope != record.Receipt.Scope || current.Receipt.State != "reserved" {
			return storeError("operation_conflict", "Check-in intent changed before completion.")
		}
		return storeWrite(dir, checkinIntentName(record.Receipt.Scope), record, storeFileLimit)
	})
}
