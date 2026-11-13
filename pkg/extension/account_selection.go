package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

type AccountSelection struct {
	ID                 string `json:"id"`
	ExpectedGeneration string `json:"expectedGeneration"`
}

type AccountSummary struct {
	ID         string `json:"id"`
	Generation string `json:"generation"`
	ExpiresAt  int64  `json:"expiresAt,omitempty"`
}

type WindowCallOptions struct {
	Account *AccountSelection `json:"account,omitempty"`
}

func (selection AccountSelection) Validate() (uint64, error) {
	if selection.ID == "" || len(selection.ID) > 256 || !utf8.ValidString(selection.ID) {
		return 0, errors.New("account selection requires a bounded account identity")
	}
	for _, char := range selection.ID {
		if char < 0x20 || char == 0x7f {
			return 0, errors.New("account selection contains an invalid identity")
		}
	}
	generation, err := strconv.ParseUint(selection.ExpectedGeneration, 10, 64)
	if err != nil || generation == 0 || strconv.FormatUint(generation, 10) != selection.ExpectedGeneration {
		return 0, errors.New("account selection requires a canonical nonzero uint64 generation")
	}
	return generation, nil
}

func (selection *AccountSelection) UnmarshalJSON(raw []byte) error {
	type plain AccountSelection
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || decoder.Decode(&value) != nil ||
		decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid account selection")
	}
	parsed := AccountSelection(value)
	if _, err := parsed.Validate(); err != nil {
		return err
	}
	*selection = parsed
	return nil
}
