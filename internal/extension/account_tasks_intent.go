package extension

import (
	"os"
	"time"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type accountTaskIntent struct {
	Receipt JournalEntry `json:"receipt"`
	Pin     string       `json:"pin"`
}

func readAccountTaskIntent(root *os.Root, operation string) (accountTaskIntent, bool, error) {
	var intent accountTaskIntent
	directory, err := storeDirectory(root, "account-tasks")
	if err != nil {
		return intent, false, err
	}
	defer directory.Close()
	found, err := storeRead(directory, storeHash(operation)+".json", storeFileLimit, &intent)
	if err == nil && found && (intent.Receipt.Scope.Operation != operation ||
		!storeValidScope(intent.Receipt.Scope) || intent.Receipt.Scope.Account == "" ||
		intent.Receipt.Scope.Attempt != 1 || intent.Pin == "" || intent.Receipt.Updated.IsZero() ||
		intent.Receipt.State != "reserved" && !storeJournalState(intent.Receipt.State)) {
		err = storeUnreadable()
	}
	return intent, found, err
}

func (store *Store) reserveAccountTask(scope sdk.Scope, pin string) error {
	return store.withStore(scope.Plugin, func(root *os.Root) error {
		if err := storeCheckGeneration(root, scope); err != nil {
			return err
		}
		if _, found, err := readAccountTaskIntent(root, scope.Operation); err != nil {
			return err
		} else if found {
			return storeError("operation_conflict", "This account task was already reserved; it cannot be replayed.")
		}
		directory, err := storeDirectory(root, "account-tasks")
		if err != nil {
			return err
		}
		defer directory.Close()
		intent := accountTaskIntent{Pin: pin, Receipt: JournalEntry{Scope: scope, State: "reserved", Updated: time.Now().UTC()}}
		return storeWrite(directory, storeHash(scope.Operation)+".json", intent, storeFileLimit)
	})
}

func (store *Store) finishAccountTask(scope sdk.Scope, pin, state string) error {
	return store.withStore(scope.Plugin, func(root *os.Root) error {
		intent, found, err := readAccountTaskIntent(root, scope.Operation)
		if err != nil {
			return err
		}
		if !found || intent.Pin != pin || intent.Receipt.State != "reserved" ||
			intent.Receipt.Scope.Account != scope.Account || intent.Receipt.Scope.Provider != scope.Provider {
			return storeError("operation_conflict", "Account task intent changed.")
		}
		directory, err := storeDirectory(root, "account-tasks")
		if err != nil {
			return err
		}
		defer directory.Close()
		intent.Receipt = JournalEntry{Scope: scope, State: state, Updated: time.Now().UTC()}
		return storeWrite(directory, storeHash(scope.Operation)+".json", intent, storeFileLimit)
	})
}
