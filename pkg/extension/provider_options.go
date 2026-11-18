package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

type ProviderOption struct {
	Key           string            `json:"key"`
	Label         string            `json:"label"`
	Description   string            `json:"description,omitempty"`
	Default       bool              `json:"default,omitempty"`
	Confirm       string            `json:"confirm,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Descriptions  map[string]string `json:"descriptions,omitempty"`
	Confirms      map[string]string `json:"confirms,omitempty"`
	FiltersModels bool              `json:"filtersModels,omitempty"`
	Placement     string            `json:"placement,omitempty"`
}

type ProviderOptionChanges map[string]bool

func (changes *ProviderOptionChanges) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || len(fields) > 32 {
		return fmt.Errorf("provider switch changes must be a bounded boolean object")
	}
	next := make(ProviderOptionChanges, len(fields))
	for key, value := range fields {
		value = bytes.TrimSpace(value)
		if !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
			return fmt.Errorf("provider switch %q must be boolean", key)
		}
		next[key] = bytes.Equal(value, []byte("true"))
	}
	*changes = next
	return nil
}

func (option *ProviderOption) UnmarshalJSON(raw []byte) error {
	type fields ProviderOption
	var decoded fields
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid provider switch declaration")
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return fmt.Errorf("provider switch must be an object")
	}
	for _, key := range []string{"default", "filtersModels"} {
		value := bytes.TrimSpace(values[key])
		if len(value) > 0 && !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
			return fmt.Errorf("provider switch %s must be boolean", key)
		}
	}
	for _, key := range []string{"key", "label", "description", "confirm", "placement", "labels", "descriptions", "confirms"} {
		if bytes.Equal(bytes.TrimSpace(values[key]), []byte("null")) {
			return fmt.Errorf("provider switch %s cannot be null", key)
		}
	}
	for _, key := range []string{"labels", "descriptions", "confirms"} {
		var words map[string]json.RawMessage
		if json.Unmarshal(values[key], &words) == nil {
			for _, word := range words {
				if bytes.Equal(bytes.TrimSpace(word), []byte("null")) {
					return fmt.Errorf("provider switch translations must be strings")
				}
			}
		}
	}
	*option = ProviderOption(decoded)
	return nil
}

func cloneProviderOptions(options []ProviderOption) []ProviderOption {
	options = slices.Clone(options)
	for index := range options {
		options[index].Labels = maps.Clone(options[index].Labels)
		options[index].Descriptions = maps.Clone(options[index].Descriptions)
		options[index].Confirms = maps.Clone(options[index].Confirms)
	}
	return options
}

func providerOptionKey(key string) bool {
	if len(key) == 0 || len(key) > 40 || key[0] < 'a' || key[0] > 'z' ||
		key == "constructor" || key == "prototype" {
		return false
	}
	for _, char := range key {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func providerOptionText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func providerOptionWords(words map[string]string, limit int) bool {
	if len(words) > 16 {
		return false
	}
	for language, text := range words {
		if len(language) == 0 || len(language) > 35 || strings.ContainsAny(language, "/\\ \t\r\n\x00") ||
			!providerOptionText(text, limit) {
			return false
		}
	}
	return true
}

func (manifest Manifest) validateProviderOptions() error {
	declared := map[string][]byte{}
	for _, provider := range manifest.Providers {
		if len(provider.Options) == 0 {
			continue
		}
		if manifest.Contract.Version.Minor < 9 || len(provider.Options) > 32 || manifest.Component != nil {
			return fmt.Errorf("provider switches require SDK API 1.9 and a root provider")
		}
		seen := map[string]bool{}
		for _, option := range provider.Options {
			if !providerOptionKey(option.Key) || seen[option.Key] || strings.TrimSpace(option.Label) == "" ||
				!providerOptionText(option.Label, 512) || !providerOptionText(option.Description, 4096) ||
				!providerOptionText(option.Confirm, 4096) ||
				!providerOptionWords(option.Labels, 512) || !providerOptionWords(option.Descriptions, 4096) ||
				!providerOptionWords(option.Confirms, 4096) || option.Placement != "" && option.Placement != "top" {
				return fmt.Errorf("invalid or duplicate provider switch %q", option.Key)
			}
			seen[option.Key] = true
			wire, err := json.Marshal(option)
			if err != nil {
				return err
			}
			if previous, exists := declared[option.Key]; exists && !bytes.Equal(previous, wire) {
				return fmt.Errorf("provider switch %q has conflicting root configuration declarations", option.Key)
			}
			declared[option.Key] = wire
		}
	}
	return nil
}

func (option ProviderOption) On(values json.RawMessage) (bool, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(values, &fields) != nil || fields == nil {
		return false, fmt.Errorf("provider switch configuration must be an object")
	}
	raw, exists := fields[option.Key]
	if !exists {
		return option.Default, nil
	}
	if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
		return false, fmt.Errorf("provider switch %q must be boolean", option.Key)
	}
	return bytes.Equal(raw, []byte("true")), nil
}

func (manifest Manifest) ValidateProviderOptionValues(values json.RawMessage) error {
	for _, provider := range manifest.Providers {
		for _, option := range provider.Options {
			if _, err := option.On(values); err != nil {
				return err
			}
		}
	}
	return nil
}
