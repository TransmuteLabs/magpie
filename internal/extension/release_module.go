package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
	"golang.org/x/mod/semver"
)

const (
	releaseModulePageSize = 100
	releaseModulePages    = 10
)

var errNoModuleRelease = errors.New("GitHub releases contain no published stable version of the selected module")

func releaseModuleVersion(tag, module string) (string, bool) {
	version, matched := strings.CutPrefix(tag, module+"-v")
	return version, matched && releaseVersion(version)
}

func releaseModuleSibling(tag, module string) bool {
	for boundary := strings.LastIndex(tag, "-v"); boundary > len(module); boundary = strings.LastIndex(tag[:boundary], "-v") {
		sibling := tag[:boundary]
		if sdk.ValidID(sibling) {
			if _, valid := releaseModuleVersion(tag, sibling); valid {
				return true
			}
		}
	}
	return false
}

func releaseMetadata(ctx context.Context, auth *releaseAuthorization, endpoint string, target any) (http.Header, error) {
	var metadata bytes.Buffer
	headers, err := auth.metadata(ctx, endpoint, &metadata)
	if err != nil {
		return nil, fmt.Errorf("read GitHub release: %w", err)
	}
	tokens := json.NewDecoder(bytes.NewReader(metadata.Bytes()))
	tokens.UseNumber()
	if err := catalogJSONValue(tokens, 0); err != nil {
		return nil, fmt.Errorf("invalid GitHub release metadata: %w", err)
	}
	if err := json.Unmarshal(metadata.Bytes(), target); err != nil {
		return nil, fmt.Errorf("invalid GitHub release metadata: %w", err)
	}
	return headers, nil
}

func releaseModuleNext(headers http.Header, endpoint string, page int) (bool, error) {
	next := false
	for _, header := range headers.Values("Link") {
		for _, item := range strings.Split(header, ",") {
			item = strings.TrimSpace(item)
			link, marked := strings.CutPrefix(item, "<")
			target, parameters, closed := strings.Cut(link, ">")
			_, attributes, err := mime.ParseMediaType("link" + parameters)
			if !marked || !closed || target == "" || err != nil || attributes["rel"] == "" {
				return false, errors.New("GitHub module pagination has malformed Link metadata")
			}
			for _, relation := range strings.Fields(attributes["rel"]) {
				if !strings.EqualFold(relation, "next") {
					continue
				}
				if next || page >= releaseModulePages || !releaseModulePageTarget(target, endpoint, page+1) {
					return false, errors.New("GitHub module continuation is not the next bounded repository page")
				}
				next = true
			}
		}
	}
	return next, nil
}

func releaseModulePageTarget(target, endpoint string, page int) bool {
	location, query, _ := strings.Cut(target, "?")
	if query != "per_page=100&page="+strconv.Itoa(page) {
		return false
	}
	if location == endpoint {
		return true
	}
	repository, canonical := strings.CutPrefix(location, "https://api.github.com/repositories/")
	id, releaseList := strings.CutSuffix(repository, "/releases")
	number, err := strconv.ParseUint(id, 10, 64)
	return canonical && releaseList && err == nil && number != 0 && id == strconv.FormatUint(number, 10)
}

func releaseResolve(ctx context.Context, auth *releaseAuthorization, endpoint, pin, module string) (githubRelease, error) {
	if module == "" || pin != "" {
		var release githubRelease
		_, err := releaseMetadata(ctx, auth, endpoint, &release)
		return release, err
	}
	var selected githubRelease
	selectedVersion := ""
	seen := map[string]string{}
	for page := 1; page <= releaseModulePages; page++ {
		var releases []githubRelease
		location := endpoint + "?per_page=100&page=" + strconv.Itoa(page)
		headers, err := releaseMetadata(ctx, auth, location, &releases)
		if err != nil {
			return githubRelease{}, err
		}
		next, err := releaseModuleNext(headers, endpoint, page)
		if err != nil {
			return githubRelease{}, err
		}
		if releases == nil {
			return githubRelease{}, errors.New("GitHub module release page must be an array")
		}
		if len(releases) > releaseModulePageSize {
			return githubRelease{}, errors.New("GitHub module release page exceeds its entry limit")
		}
		for _, release := range releases {
			if release.Draft || release.Prerelease || !strings.HasPrefix(release.Tag, module+"-v") {
				continue
			}
			version, valid := releaseModuleVersion(release.Tag, module)
			if !valid {
				if releaseModuleSibling(release.Tag, module) {
					continue
				}
				return githubRelease{}, errors.New("GitHub module release has an invalid semantic version tag")
			}
			if semver.Prerelease("v"+version) != "" {
				continue
			}
			key := semver.Canonical("v" + version)
			if _, exists := seen[key]; exists {
				return githubRelease{}, errors.New("GitHub module releases contain duplicate or ambiguous semantic versions")
			}
			seen[key] = release.Tag
			if selectedVersion == "" || semver.Compare("v"+version, "v"+selectedVersion) > 0 {
				selected, selectedVersion = release, version
			}
		}
		if len(releases) < releaseModulePageSize && !next {
			if selectedVersion == "" {
				return githubRelease{}, errNoModuleRelease
			}
			return selected, nil
		}
	}
	return githubRelease{}, errors.New("GitHub module release listing is incomplete at its page limit; use an explicit module tag pin")
}
