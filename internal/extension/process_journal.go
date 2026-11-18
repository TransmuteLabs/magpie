package extension

import (
	"errors"
	"os"

	sdk "github.com/yetone/magpie/pkg/extension"
)

type processReceipt struct {
	Scope       sdk.Scope `json:"scope"`
	Recipe      string    `json:"recipe"`
	Profile     string    `json:"profile"`
	Revision    string    `json:"revision"`
	Package     string    `json:"package"`
	Config      string    `json:"config"`
	Incarnation string    `json:"incarnation"`
	State       string    `json:"state"`
	WriteSeq    uint64    `json:"writeSeq"`
	WriteID     string    `json:"writeId,omitempty"`
	Written     int       `json:"written"`
	Joined      bool      `json:"joined,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

func (parent *ownedProcess) journalJoined() error {
	return parent.lease.manager.store.withStore(parent.scope.Plugin, func(root *os.Root) error {
		directory, err := storeDirectory(root, "processes")
		if err != nil {
			return err
		}
		defer directory.Close()
		name := storeHash(parent.scope.Operation) + ".json"
		var receipt processReceipt
		found, err := storeRead(directory, name, storeFileLimit, &receipt)
		if err != nil || !found {
			return err
		}
		if receipt.Scope != parent.scope || receipt.Incarnation != parent.incarnation {
			return errors.New("owned process final receipt changed")
		}
		receipt.Joined, receipt.Reason = true, parent.reason
		return storeWrite(directory, name, receipt, storeFileLimit)
	})
}

func (parent *ownedProcess) journal(state, writeID string, seq uint64, written int, first bool) error {
	receipt := processReceipt{
		Scope: parent.scope, Recipe: parent.launch.Recipe.ID, Profile: parent.launch.Profile,
		Revision: parent.launch.Revision, Package: parent.lease.Revision(), Config: parent.lease.ConfigVersion(),
		Incarnation: parent.incarnation, State: state, WriteSeq: seq, WriteID: writeID, Written: written,
	}
	return parent.lease.manager.store.withStore(parent.scope.Plugin, func(root *os.Root) error {
		directory, err := storeDirectory(root, "processes")
		if err != nil {
			return err
		}
		defer directory.Close()
		name := storeHash(parent.scope.Operation) + ".json"
		var previous processReceipt
		found, err := storeRead(directory, name, storeFileLimit, &previous)
		if err != nil {
			return err
		}
		if first && found || !first && (!found || previous.Scope != parent.scope ||
			previous.Incarnation != parent.incarnation || seq < previous.WriteSeq) {
			return &OutcomeError{Outcome: "uncertain", Cause: errors.New("owned process delivery cannot be replayed")}
		}
		return storeWrite(directory, name, receipt, storeFileLimit)
	})
}
