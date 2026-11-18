package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MethodProcessOpen     = "process.open"
	MethodProcessRead     = "process.read"
	MethodProcessWrite    = "process.write"
	MethodProcessStdinEOF = "process.stdinEOF"
	MethodProcessClose    = "process.close"
	MaxProcessArgv        = 128
	MaxProcessArgvBytes   = 64 << 10
	ProcessLaneCount      = 4
)

type AgentProcessRecipe struct {
	ID    string   `json:"id"`
	Agent string   `json:"agent"`
	Argv  []string `json:"argv"`
}

type ProcessOpenRequest struct {
	RecipeID string `json:"recipeId"`
}

type ProcessLimits struct {
	MaxChunkBytes int `json:"maxChunkBytes"`
	QueueChunks   int `json:"queueChunks"`
}

type ProcessOpenResult struct {
	Handle      string        `json:"handle"`
	Incarnation string        `json:"incarnation"`
	Limits      ProcessLimits `json:"limits"`
}

type ProcessReadRequest struct {
	Handle   string `json:"handle"`
	Pipe     string `json:"pipe"`
	Seq      string `json:"seq"`
	MaxBytes int    `json:"maxBytes"`
}

type ProcessReadResult struct {
	Seq   string `json:"seq"`
	Bytes []byte `json:"bytes"`
	EOF   bool   `json:"eof"`
}

type ProcessWriteRequest struct {
	Handle  string `json:"handle"`
	WriteID string `json:"writeId"`
	Seq     string `json:"seq"`
	Bytes   []byte `json:"bytes"`
}

type ProcessWriteResult struct {
	Written  int    `json:"written"`
	Delivery string `json:"delivery"`
}

type ProcessHandleRequest struct {
	Handle string `json:"handle"`
}

type ProcessCloseResult struct {
	Joined   bool   `json:"joined"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Reason   string `json:"reason"`
}

type ProcessOutcome struct {
	Outcome string `json:"outcome"`
}

func ValidateAgentProcessRecipe(recipe AgentProcessRecipe) error {
	if !validID(recipe.ID) || !validID(recipe.Agent) || len(recipe.Argv) == 0 || len(recipe.Argv) > MaxProcessArgv {
		return errors.New("process recipe requires a declared agent and bounded literal arguments")
	}
	total := 0
	for _, argument := range recipe.Argv {
		total += len(argument) + 1
		if !utf8.ValidString(argument) || strings.ContainsRune(argument, 0) || total > MaxProcessArgvBytes {
			return errors.New("process recipe arguments exceed their byte contract")
		}
	}
	return nil
}

func IsProcessHostMethod(method string) bool {
	switch method {
	case MethodProcessOpen, MethodProcessRead, MethodProcessWrite, MethodProcessStdinEOF, MethodProcessClose:
		return true
	}
	return false
}

func ProcessHostLane(method string, raw json.RawMessage) (int, error) {
	switch method {
	case MethodProcessOpen:
		var request ProcessOpenRequest
		if processParams(raw, &request) && validID(request.RecipeID) {
			return 3, nil
		}
	case MethodProcessRead:
		var request ProcessReadRequest
		if processParams(raw, &request) && processToken(request.Handle) &&
			processSequence(request.Seq) && request.MaxBytes > 0 && request.MaxBytes <= MaxChunkBytes {
			switch request.Pipe {
			case "stdout":
				return 0, nil
			case "stderr":
				return 1, nil
			}
		}
	case MethodProcessWrite:
		var request ProcessWriteRequest
		if processParams(raw, &request) && processToken(request.Handle) && processToken(request.WriteID) &&
			processSequence(request.Seq) && len(request.Bytes) > 0 && len(request.Bytes) <= MaxChunkBytes {
			return 2, nil
		}
	case MethodProcessStdinEOF, MethodProcessClose:
		var request ProcessHandleRequest
		if processParams(raw, &request) && processToken(request.Handle) {
			return 3, nil
		}
	}
	return 0, &Error{Code: "invalid_params", Message: "invalid bounded process callback"}
}

func processSequence(value string) bool {
	number, err := strconv.ParseUint(value, 10, 64)
	return err == nil && number != 0 && number != ^uint64(0) && strconv.FormatUint(number, 10) == value
}

func processToken(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, func(character rune) bool { return unicode.IsControl(character) || unicode.IsSpace(character) })
}

func processParams(raw json.RawMessage, target any) bool {
	if len(raw) == 0 || len(raw) > MaxHostBytes {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for decoder.More() {
		key, err := decoder.Token()
		name, valid := key.(string)
		if err != nil || !valid || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	if _, err := decoder.Token(); err != nil || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	return decodeParams(raw, target) == nil
}
