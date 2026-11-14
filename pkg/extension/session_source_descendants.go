package extension

import (
	"errors"
	"strings"
)

func validateSessionSourceDescendants(rule *SessionSourceDescendants) error {
	if rule.Kind != "journal-stem-jsonl" || len(rule.ExcludeDirectories) > 16 {
		return errors.New("invalid session source descendant rule")
	}
	seen := map[string]bool{}
	for _, name := range rule.ExcludeDirectories {
		if !validID(name) || strings.Contains(name, "/") || seen[name] {
			return errors.New("invalid session source excluded directory")
		}
		seen[name] = true
	}
	return nil
}

func SessionSourceDescendantPath(rule *SessionSourceDescendants, journal, name string) bool {
	if rule == nil || validateSessionSourceDescendants(rule) != nil || !SessionSourcePath(journal) ||
		!SessionSourcePath(name) || !strings.HasSuffix(journal, ".jsonl") {
		return false
	}
	relative, found := strings.CutPrefix(name, strings.TrimSuffix(journal, ".jsonl")+"/")
	if !found || !strings.HasSuffix(relative, ".jsonl") {
		return false
	}
	parts := strings.Split(relative, "/")
	for _, directory := range parts[:len(parts)-1] {
		for _, excluded := range rule.ExcludeDirectories {
			if directory == excluded {
				return false
			}
		}
	}
	return true
}

func validateSessionSourceDescendantRequest(manifest Manifest, source SessionSourceDescriptor, request SessionSourceReadRequest, documents map[string]bool) error {
	if request.Descendants == nil {
		return nil
	}
	if manifest.Contract.Version.Minor < 9 || source.Descendants == nil || len(request.Descendants) >= 128 {
		return errors.New("session source descendant acquisition was not declared")
	}
	seen := map[string]bool{}
	for _, name := range request.Descendants {
		if seen[name] || !documents[name] || !SessionSourceDescendantPath(source.Descendants, request.Locator.Path, name) {
			return errors.New("session source descendant is outside its admitted documents")
		}
		seen[name] = true
	}
	return nil
}

func validateSessionSourceIdentities(request SessionSourceReadRequest, identities []SessionSourceIdentity) error {
	if len(identities) != len(request.Descendants) || len(identities) >= 128 {
		return errors.New("session source identities must cover its admitted descendants")
	}
	admitted := map[string]bool{}
	for _, name := range request.Descendants {
		if !SessionSourcePath(name) || admitted[name] {
			return errors.New("invalid admitted session source descendant")
		}
		admitted[name] = true
	}
	ids := map[string]bool{request.Locator.SessionID: true}
	for _, identity := range identities {
		if !admitted[identity.Path] || !validSessionSourceID(identity.SessionID) || ids[identity.SessionID] {
			return errors.New("invalid or duplicate session source descendant identity")
		}
		delete(admitted, identity.Path)
		ids[identity.SessionID] = true
	}
	return nil
}
