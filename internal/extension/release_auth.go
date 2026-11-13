package extension

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

const (
	releaseCredentialLimit  = 16 << 10
	releaseCredentialWait   = 5 * time.Second
	releaseCredentialPrompt = "GitHub release access is unavailable. Refresh gh auth login --hostname github.com, or configure an HTTPS Git credential or GH_TOKEN/GITHUB_TOKEN with repository Contents read access. SSH-only Git authentication cannot access the Releases API."
)

type releaseTokenResolver func(context.Context, string) (string, error)

type releaseCredentialCommand struct {
	Name  string
	Args  []string
	Input string
	Env   []string
}

type releaseCredentialRunner func(context.Context, releaseCredentialCommand) ([]byte, error)

type releaseAuthorization struct {
	client        *http.Client
	repository    string
	resolver      releaseTokenResolver
	token         string
	authenticated bool
}

type releaseHTTPStatusError struct {
	status int
	direct bool
}

type releaseHTTPRequestError struct{ cause error }

func (failure *releaseHTTPRequestError) Error() string { return "release HTTP request failed" }
func (failure *releaseHTTPRequestError) Unwrap() error { return failure.cause }

func (failure *releaseHTTPStatusError) Error() string {
	return fmt.Sprintf("release HTTP status %d", failure.status)
}

func releaseAuthChallenge(err error) bool {
	var failure *releaseHTTPStatusError
	return errors.As(err, &failure) && (failure.status == http.StatusUnauthorized ||
		failure.status == http.StatusForbidden || failure.status == http.StatusNotFound)
}

func releaseAuthTarget(location, repository string) bool {
	parsed, err := url.Parse(location)
	return err == nil && parsed.Scheme == "https" && parsed.Host == "api.github.com" &&
		parsed.User == nil && parsed.Opaque == "" && parsed.RawQuery == "" && parsed.Fragment == "" &&
		parsed.RawPath == "" && path.Clean(parsed.Path) == parsed.Path &&
		strings.HasPrefix(parsed.Path, "/repos/"+repository+"/releases/")
}

func (auth *releaseAuthorization) metadata(ctx context.Context, endpoint string, output *bytes.Buffer) error {
	_, err := releaseDownloadRequest(ctx, auth.client, endpoint, output, releaseMetadataLimit, "", auth.repository, false)
	if !releaseAuthChallenge(err) {
		return err
	}
	var failure *releaseHTTPStatusError
	if !errors.As(err, &failure) || !failure.direct || !releaseAuthTarget(endpoint, auth.repository) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if auth.resolver == nil {
		return errors.New(releaseCredentialPrompt)
	}
	token, lookupErr := auth.resolver(ctx, auth.repository)
	if err := ctx.Err(); err != nil {
		return err
	}
	if lookupErr != nil || !releaseCredentialToken(token) {
		return errors.New(releaseCredentialPrompt)
	}
	auth.token, auth.authenticated = token, true
	output.Reset()
	_, err = releaseDownloadRequest(ctx, auth.client, endpoint, output, releaseMetadataLimit, token, auth.repository, false)
	if releaseAuthChallenge(err) {
		return errors.New(releaseCredentialPrompt)
	}
	return err
}

func (auth *releaseAuthorization) asset(ctx context.Context, asset releaseAsset, output io.Writer, limit int64) (int64, error) {
	location := asset.URL
	token := ""
	if asset.ID > 0 {
		location = "https://api.github.com/repos/" + auth.repository + "/releases/assets/" + strconv.FormatInt(asset.ID, 10)
		if releaseAuthTarget(location, auth.repository) {
			token = auth.token
		}
	} else if auth.authenticated {
		return 0, errors.New("authenticated GitHub releases require asset IDs for the archive and checksums.txt")
	}
	size, err := releaseDownloadRequest(ctx, auth.client, location, output, limit, token, auth.repository, asset.ID > 0)
	if releaseAuthChallenge(err) && asset.ID > 0 {
		return size, errors.New(releaseCredentialPrompt)
	}
	return size, err
}

func defaultReleaseToken(ctx context.Context, repository string) (string, error) {
	return resolveReleaseToken(ctx, repository, os.Getenv, runReleaseCredential)
}

func resolveReleaseToken(ctx context.Context, repository string, getenv func(string) string, run releaseCredentialRunner) (string, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || !releaseSegment(parts[0], false) || !releaseSegment(parts[1], true) || getenv == nil || run == nil {
		return "", errors.New(releaseCredentialPrompt)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		token := strings.TrimSpace(getenv(name))
		if token != "" {
			if !releaseCredentialToken(token) {
				return "", errors.New(releaseCredentialPrompt)
			}
			return token, nil
		}
	}
	commands := []releaseCredentialCommand{
		{Name: "gh", Args: []string{"auth", "token", "--hostname", "github.com"}, Env: []string{
			"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_NO_EXTENSION_UPDATE_NOTIFIER=1",
		}},
		{Name: "git", Args: []string{"-c", "credential.interactive=false", "-c", "core.askPass=", "credential", "fill"},
			Input: "protocol=https\nhost=github.com\npath=" + repository + ".git\n\n",
			Env:   []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_ASKPASS=", "SSH_ASKPASS="}},
	}
	for _, command := range commands {
		lookup, cancel := context.WithTimeout(ctx, releaseCredentialWait)
		output, err := run(lookup, command)
		cancel()
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err != nil || len(output) > releaseCredentialLimit {
			continue
		}
		token := strings.TrimSpace(string(output))
		if command.Name == "git" {
			token = releaseGitCredential(output, repository)
		}
		if releaseCredentialToken(token) {
			return token, nil
		}
	}
	return "", errors.New(releaseCredentialPrompt)
}

func releaseCredentialToken(token string) bool {
	if len(token) == 0 || len(token) > releaseCredentialLimit {
		return false
	}
	for _, char := range token {
		if char <= 0x20 || char > 0x7e {
			return false
		}
	}
	return true
}

func releaseGitCredential(output []byte, repository string) string {
	var token string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || seen[key] {
			return ""
		}
		seen[key] = true
		switch key {
		case "protocol":
			if value != "https" {
				return ""
			}
		case "host":
			if value != "github.com" {
				return ""
			}
		case "path":
			if value != repository+".git" {
				return ""
			}
		case "password":
			token = value
		}
	}
	return token
}

type releaseCredentialOutput struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (output *releaseCredentialOutput) Write(data []byte) (int, error) {
	if len(data) > releaseCredentialLimit-output.buffer.Len() {
		output.cancel()
		return 0, errors.New("GitHub credential output exceeds its limit")
	}
	return output.buffer.Write(data)
}

func releaseCredentialEnv(base, overrides []string) []string {
	env := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		replaced := false
		for _, override := range overrides {
			key, _, _ := strings.Cut(override, "=")
			replaced = replaced || strings.EqualFold(name, key)
		}
		if !replaced {
			env = append(env, entry)
		}
	}
	return append(env, overrides...)
}

func runReleaseCredential(ctx context.Context, command releaseCredentialCommand) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, releaseCredentialWait)
	defer cancel()
	output := &releaseCredentialOutput{cancel: cancel}
	child := proc.CommandContext(ctx, command.Name, command.Args...)
	child.Stdin = strings.NewReader(command.Input)
	child.Stdout, child.Stderr = output, io.Discard
	child.Env = releaseCredentialEnv(os.Environ(), command.Env)
	child.WaitDelay = 250 * time.Millisecond
	if err := child.Run(); err != nil {
		return nil, errors.New("GitHub credential helper did not return a usable token")
	}
	return bytes.Clone(output.buffer.Bytes()), nil
}
