package extension

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"sync"
)

const MaskingRules Capability = "masking-rules"

type MaskingRule struct {
	Kind   string `json:"kind"`
	Prefix string `json:"prefix,omitempty"`
	Regex  string `json:"regex,omitempty"`
}

type MaskingRuleSet struct {
	ID    string        `json:"id"`
	Rules []MaskingRule `json:"rules"`
}

func decodeMaskingObject(data []byte, target any, fields ...string) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	for field := range members {
		if !slices.Contains(fields, field) {
			return errors.New("unknown declarative masking field")
		}
	}
	return json.Unmarshal(data, target)
}

func (rule *MaskingRule) UnmarshalJSON(data []byte) error {
	type plain MaskingRule
	return decodeMaskingObject(data, (*plain)(rule), "kind", "prefix", "regex")
}

func (set *MaskingRuleSet) UnmarshalJSON(data []byte) error {
	type plain MaskingRuleSet
	return decodeMaskingObject(data, (*plain)(set), "id", "rules")
}

var (
	maskingKind = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^SDK_[A-Z](?:[A-Z0-9_]{0,18}[A-Z0-9])?$`)
	})
	maskingPrefix = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_:/.-]{2,63}$`)
	})
	maskingRegex = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_:/-]{2,63})\[([A-Za-z0-9_-]{1,64})\]\{([1-9][0-9]{0,2})(?:,([1-9][0-9]{0,2}))?\}$`)
	})
)

func (rule MaskingRule) Compile() (*regexp.Regexp, error) {
	invalid := errors.New("invalid declarative masking rule")
	if !maskingKind().MatchString(rule.Kind) || (rule.Prefix == "") == (rule.Regex == "") {
		return nil, invalid
	}
	if rule.Prefix != "" {
		if !maskingPrefix().MatchString(rule.Prefix) {
			return nil, invalid
		}
		return regexp.Compile(regexp.QuoteMeta(rule.Prefix) + `[A-Za-z0-9_-]{8,}`)
	}
	parts := maskingRegex().FindStringSubmatch(rule.Regex)
	if len(parts) == 0 {
		return nil, invalid
	}
	low, _ := strconv.Atoi(parts[3])
	high := low
	if parts[4] != "" {
		high, _ = strconv.Atoi(parts[4])
	}
	if low < 4 || high < low || high > 128 {
		return nil, invalid
	}
	compiled, err := regexp.Compile(rule.Regex)
	if err != nil {
		return nil, invalid
	}
	return compiled, nil
}

func (manifest Manifest) validateMaskingRules() error {
	declared := manifest.Contract.Has(MaskingRules)
	if !declared && len(manifest.MaskingRuleSets) == 0 {
		return nil
	}
	if !declared || manifest.Contract.Version.Minor < 15 || manifest.Component != nil ||
		len(manifest.MaskingRuleSets) == 0 || len(manifest.MaskingRuleSets) > 8 {
		return errors.New("masking rule sets require a primary API 1.15 masking-rules declaration")
	}
	sets := map[string]bool{}
	for _, set := range manifest.MaskingRuleSets {
		if !validID(set.ID) || sets[set.ID] || len(set.Rules) == 0 || len(set.Rules) > 8 {
			return errors.New("invalid declarative masking rule set")
		}
		sets[set.ID] = true
		kinds := map[string]bool{}
		for _, rule := range set.Rules {
			if _, err := rule.Compile(); err != nil || kinds[rule.Kind] {
				return errors.New("invalid or duplicate declarative masking rule")
			}
			kinds[rule.Kind] = true
		}
	}
	return nil
}
