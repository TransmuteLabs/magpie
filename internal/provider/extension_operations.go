package provider

import (
	"errors"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	sdk "github.com/yetone/magpie/pkg/extension"
)

func (p Provider) ExtensionSupportsOperation(operation sdk.ProviderOperation) bool {
	return p.IsExtension() && p.Account.extension.descriptor.SupportsOperation(operation)
}

func extensionRequestOperation(request *http.Request, descriptor sdk.ProviderDescriptor) (string, sdk.ProviderOperation, error) {
	endpoint := request.URL.EscapedPath()
	if path.Clean(endpoint) != endpoint {
		return "", "", errors.New("extension provider request has an ambiguous endpoint")
	}
	endpoint = strings.TrimPrefix(endpoint, "/v1")
	switch request.Method + " " + endpoint {
	case "POST /chat/completions":
		return "chat", "", nil
	case "POST /responses":
		return "responses", "", nil
	case "POST /messages", "POST /messages/count_tokens":
		return "anthropic", "", nil
	}
	if request.Method == http.MethodPost {
		gemini := strings.TrimPrefix(request.URL.Path, "/v1beta")
		if model, method, found := strings.Cut(strings.TrimPrefix(gemini, "/models/"), ":"); found &&
			strings.HasPrefix(gemini, "/models/") && model != "" &&
			slices.Contains([]string{"generateContent", "streamGenerateContent", "countTokens"}, method) {
			return "gemini", "", nil
		}
	}
	var operation sdk.ProviderOperation
	switch request.Method + " " + endpoint {
	case "POST /embeddings":
		operation = sdk.OperationEmbeddings
	case "POST /rerank":
		operation = sdk.OperationRerank
	case "POST /images/generations":
		operation = sdk.OperationImageGenerate
	case "POST /images/edits":
		operation = sdk.OperationImageEdit
	case "POST /audio/transcriptions":
		operation = sdk.OperationAudioTranscribe
	case "POST /audio/translations":
		operation = sdk.OperationAudioTranslate
	case "POST /audio/speech":
		operation = sdk.OperationAudioSpeech
	case "POST /videos":
		operation = sdk.OperationVideoCreate
	default:
		parts := strings.Split(strings.TrimPrefix(endpoint, "/"), "/")
		if len(parts) >= 2 && parts[0] == "videos" && parts[1] != "" {
			identity, err := url.PathUnescape(parts[1])
			if err != nil || identity == "." || identity == ".." {
				return "", "", errors.New("extension video request has an ambiguous identifier")
			}
			switch {
			case request.Method == http.MethodGet && len(parts) == 2:
				operation = sdk.OperationVideoGet
			case request.Method == http.MethodDelete && len(parts) == 2:
				operation = sdk.OperationVideoDelete
			case request.Method == http.MethodGet && len(parts) == 3 && parts[2] == "content":
				operation = sdk.OperationVideoContent
			case request.Method == http.MethodPost && len(parts) == 3 && parts[2] == "remix":
				operation = sdk.OperationVideoRemix
			}
		}
	}
	if operation == "" || !descriptor.SupportsOperation(operation) {
		return "", "", errors.New("extension provider did not declare the requested operation")
	}
	return "chat", operation, nil
}
