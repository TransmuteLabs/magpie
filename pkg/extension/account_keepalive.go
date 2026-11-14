package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const (
	MinAccountKeepAliveLeadMs int64 = 60_000
	MaxAccountKeepAliveLeadMs int64 = 3_600_000
)

type AccountKeepAliveDescriptor struct {
	LeadMs int64 `json:"leadMs,omitempty"`
}

func (value *AccountKeepAliveDescriptor) Lead() time.Duration {
	if value == nil {
		return 0
	}
	lead := value.LeadMs
	if lead == 0 {
		lead = MinAccountKeepAliveLeadMs
	}
	if lead < MinAccountKeepAliveLeadMs || lead > MaxAccountKeepAliveLeadMs {
		return 0
	}
	return time.Duration(lead) * time.Millisecond
}

func (value *AccountKeepAliveDescriptor) UnmarshalJSON(raw []byte) error {
	var wire struct {
		LeadMs json.RawMessage `json:"leadMs"`
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("account keep-alive must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid account keep-alive descriptor")
	}
	var lead int64
	if len(wire.LeadMs) > 0 && (json.Unmarshal(wire.LeadMs, &lead) != nil ||
		lead < MinAccountKeepAliveLeadMs || lead > MaxAccountKeepAliveLeadMs) {
		return fmt.Errorf("account keep-alive lead must be an integer from one minute through one hour")
	}
	value.LeadMs = lead
	return nil
}

func (manifest Manifest) validateAccountKeepAlives() error {
	for _, provider := range manifest.Providers {
		if provider.KeepAlive == nil {
			continue
		}
		if manifest.Contract.Version.Minor < 8 || !manifest.Contract.Has(AccountAuth) ||
			manifest.Component != nil || provider.KeepAlive.Lead() == 0 {
			return fmt.Errorf("account keep-alive requires SDK API 1.8, account auth and a bounded lead")
		}
	}
	return nil
}
