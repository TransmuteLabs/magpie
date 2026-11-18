package redact

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestDeclarativeMaskingPreservesNativeActionsAndRestores(t *testing.T) {
	const secret = "acme-abcdef123456"
	const own = "acme-other987654"
	o := Options{
		Words:          []string{own},
		Actions:        map[string]string{"TERM": Warn},
		ExtensionRules: []DeclarativeRule{{Kind: "SDK_TOKEN", Pattern: regexp.MustCompile(`acme-[0-9a-z]{8,}`)}},
	}
	masked, n := Mask(secret+" "+own, o)
	if n != 1 || !strings.Contains(masked, "{{SDK_TOKEN_") || !strings.Contains(masked, own) ||
		Restore(masked, false) != secret+" "+own {
		t.Fatalf("did not preserve user policy and restore the SDK match: %d %s", n, masked)
	}
	if got, n := Mask(secret, Options{Secrets: true}); n != 0 || got != secret {
		t.Fatalf("retired declarative rule migrated into native learned masking: %d %s", n, got)
	}
}

func TestDeclarativeMaskingRepairsSignedEchoWithoutMaskingNewContent(t *testing.T) {
	const token = "acme-abcdef7123456"
	const vendor = "acme-vendor8123456"
	o := Options{ExtensionRules: []DeclarativeRule{{Kind: "SDK_TOKEN", Pattern: regexp.MustCompile(`acme-[0-9a-z]{8,}`)}}}
	first, _ := Mask(token, o)
	if !strings.Contains(first, "{{SDK_TOKEN_") {
		t.Fatal(first)
	}
	msg := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"` + token + " " + vendor + `","signature":"signed-fixture"}]}]}`)
	out, _ := MaskJSON(msg, o)
	if !strings.Contains(string(out), first+" "+vendor) || strings.Contains(string(out), token) {
		t.Fatal("selected SDK rule did not repair only the prior signed echo", string(out))
	}
}

func TestDeclarativeMaskingDoesNotRemaskSignedTextAcrossRuleSets(t *testing.T) {
	const value = "acme-signedvalue1234"
	rule := DeclarativeRule{Kind: "SDK_TOKEN", Pattern: regexp.MustCompile(`acme-[0-9a-z]{8,}`)}
	a := Options{ExtensionRules: []DeclarativeRule{rule}, ExtensionScope: DeclarativeScope{Selection: "module-a/team", Revision: "r1", EnableGeneration: "g1"}}
	masked, n := Mask(value, a)
	if n != 1 || Restore(masked, false) != value {
		t.Fatalf("fixture did not mint a reversible placeholder: %q", masked)
	}
	signed := []byte(`{ "messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"` + value + `", "signature":"signed-fixture"}]}] }`)
	for _, scope := range []DeclarativeScope{
		{Selection: "module-a/other", Revision: "r1", EnableGeneration: "g1"},
		{Selection: "module-b/team", Revision: "r1", EnableGeneration: "g1"},
		{Selection: "module-a/team", Revision: "r2", EnableGeneration: "g1"},
		{Selection: "module-a/team", Revision: "r1", EnableGeneration: "g2"},
	} {
		b := a
		b.ExtensionScope = scope
		if got, n := MaskJSON(signed, b); n != 0 || !bytes.Equal(got, signed) {
			t.Errorf("scope %+v changed signed bytes: %d %s", scope, n, got)
		}
	}
	if got, n := MaskJSON(signed, a); n != 1 || !bytes.Contains(got, []byte(masked)) {
		t.Fatalf("same-generation repair disappeared: %d %s", n, got)
	}
	b := a
	b.ExtensionScope.Selection = "module-b/team"
	other, n := Mask(value, b)
	if n != 1 || other == masked || Restore(other, false) != value {
		t.Fatalf("second issuer did not get a distinct reversible placeholder: %d %q", n, other)
	}
	if got, n := MaskJSON(signed, a); n != 1 || !bytes.Contains(got, []byte(masked)) {
		t.Fatalf("a second issuer erased the first issuer's echo: %d %s", n, got)
	}
	if got, n := MaskJSON(signed, b); n != 1 || !bytes.Contains(got, []byte(other)) {
		t.Fatalf("current issuer could not repair its echo: %d %s", n, got)
	}
	for _, scope := range []DeclarativeScope{
		{Selection: "module-c/team", Revision: "r1", EnableGeneration: "g1"},
		{Selection: "module-a/team", Revision: "r1", EnableGeneration: "g2"},
	} {
		unissued := a
		unissued.ExtensionScope = scope
		if got, n := MaskJSON(signed, unissued); n != 0 || !bytes.Equal(got, signed) {
			t.Errorf("unissued scope %+v changed signed bytes: %d %s", scope, n, got)
		}
	}
}

func TestDeclarativeSignedEchoAcrossManyGenerations(t *testing.T) {
	const value = "acme-boundedissuer1234"
	rule := DeclarativeRule{Kind: "SDK_TOKEN", Pattern: regexp.MustCompile(`acme-[0-9a-z]{8,}`)}
	opts := func(i int) Options {
		return Options{
			ExtensionRules: []DeclarativeRule{rule},
			ExtensionScope: DeclarativeScope{Selection: "module/team", Revision: "r1", EnableGeneration: strconv.Itoa(i)},
		}
	}
	signed := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"` + value + `","signature":"signed-fixture"}]}]}`)
	var tags [12]string
	for i := range tags {
		if got, n := MaskJSON(signed, opts(i)); n != 0 || !bytes.Equal(got, signed) {
			t.Errorf("unissued generation %d changed signed bytes: %d %s", i, n, got)
		}
		got, n := Mask(value, opts(i))
		if n != 1 || slices.Contains(tags[:i], got) || Restore(got, false) != value {
			t.Errorf("generation %d did not issue a distinct reversible placeholder: %d %s", i, n, got)
		}
		tags[i] = got
	}
	for i, p := range tags {
		if got, n := MaskJSON(signed, opts(i)); n != 1 || !bytes.Contains(got, []byte(p)) {
			t.Errorf("generation %d could not repair its signed echo: %d %s", i, n, got)
		}
	}
}

func TestDeclarativePlaceholderCollisionKeepsExistingEntry(t *testing.T) {
	const kind, value = "SDK_TOKEN", "acme-collision1234"
	scope := DeclarativeScope{Selection: "test/main"}
	other := DeclarativeScope{Selection: "test/other"}
	for _, tc := range []struct {
		name, stored string
		owner        *DeclarativeScope
	}{
		{"native", value, nil},
		{"other scope", value, &other},
		{"other value", "acme-different1234", &scope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := declarativeTag(kind, value, scope)
			mu.Lock()
			values[p] = tc.stored
			if tc.owner != nil {
				declarativeValues[p] = declarativeIssuance{scope: *tc.owner}
			}
			mu.Unlock()
			defer func() {
				mu.Lock()
				defer mu.Unlock()
				if r := recover(); r != "declarative placeholder identity collision" {
					t.Errorf("expected collision rejection, got %v", r)
				}
				owner, sdk := declarativeValues[p]
				if values[p] != tc.stored || sdk != (tc.owner != nil) || tc.owner != nil && owner.scope != *tc.owner {
					t.Error("collision overwrote existing placeholder")
				}
				delete(values, p)
				delete(declarativeValues, p)
			}()
			declarativePlaceholder(kind, value, scope)
		})
	}
}

func TestDeclarativePlaceholderEvictionRetiresIssuance(t *testing.T) {
	// Exercise the real eviction procedure with an isolated small store.
	mu.Lock()
	oldValues, oldDeclarative, oldUsed, oldShapes := values, declarativeValues, used, shapes
	values, declarativeValues, used, shapes = map[string]string{}, map[string]declarativeIssuance{}, map[string]uint64{}, map[string]string{}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		values, declarativeValues, used, shapes = oldValues, oldDeclarative, oldUsed, oldShapes
		reindexKnown()
	})
	const kind, value = "SDK_TOKEN", "acme-eviction1234"
	a := DeclarativeScope{Selection: "test/a"}
	b := DeclarativeScope{Selection: "test/b"}
	first := declarativePlaceholder(kind, value, a)
	second := declarativePlaceholder(kind, value, b)
	mu.Lock()
	evictValues(2)
	_, stale := declarativeValues[first]
	live := declarativeIssued(second, value, b, clock)
	mu.Unlock()
	if stale || !live || Restore(first, false) != first || Restore(second, false) != value {
		t.Fatal("eviction failed to retire the older value and its issuance together")
	}
	rules := []DeclarativeRule{{Kind: kind, Pattern: regexp.MustCompile(`acme-[a-z0-9]{8,}`)}}
	if len(knownExtensionSpans(value, rules, nil, a, clock)) != 0 || len(knownExtensionSpans(value, rules, nil, b, clock)) != 1 {
		t.Fatal("signed echo authority did not follow eviction")
	}
}

func TestDeclarativeSignedEchoCannotUseIssuanceFromSameBody(t *testing.T) {
	const value = "acme-crossscope1234"
	rule := DeclarativeRule{Kind: "SDK_TOKEN", Pattern: regexp.MustCompile(`acme-[a-z0-9]{8,}`)}
	a := Options{ExtensionRules: []DeclarativeRule{rule}, ExtensionScope: DeclarativeScope{Selection: "a/team"}}
	old, _ := Mask(value, a)
	b := a
	b.ExtensionScope.Selection = "b/team"
	signed := `{"type":"thinking","thinking":"` + value + `","signature":"signed-fixture"}`
	body := []byte(`{"messages":[{"role":"user","content":"` + value + `"},{"role":"assistant","content":[` + signed + `]}]}`)
	out, n := MaskJSON(body, b)
	if n != 1 || !bytes.Contains(out, []byte(signed)) || bytes.Contains(out, []byte(old)) ||
		!bytes.Contains(out, []byte(declarativeTag(rule.Kind, value, b.ExtensionScope))) {
		t.Fatalf("new unsigned issuance improperly changed the signed bytes: %d %s", n, out)
	}
}
