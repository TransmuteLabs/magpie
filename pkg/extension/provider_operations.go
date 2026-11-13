package extension

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"
)

type ProviderOperation string

const (
	OperationEmbeddings      ProviderOperation = "embeddings"
	OperationRerank          ProviderOperation = "rerank"
	OperationImageGenerate   ProviderOperation = "images.generate"
	OperationImageEdit       ProviderOperation = "images.edit"
	OperationAudioTranscribe ProviderOperation = "audio.transcribe"
	OperationAudioTranslate  ProviderOperation = "audio.translate"
	OperationAudioSpeech     ProviderOperation = "audio.speech"
	OperationVideoCreate     ProviderOperation = "videos.create"
	OperationVideoGet        ProviderOperation = "videos.get"
	OperationVideoContent    ProviderOperation = "videos.content"
	OperationVideoDelete     ProviderOperation = "videos.delete"
	OperationVideoRemix      ProviderOperation = "videos.remix"
)

func (operation ProviderOperation) Valid() bool {
	switch operation {
	case OperationEmbeddings, OperationRerank, OperationImageGenerate, OperationImageEdit,
		OperationAudioTranscribe, OperationAudioTranslate, OperationAudioSpeech,
		OperationVideoCreate, OperationVideoGet, OperationVideoContent, OperationVideoDelete, OperationVideoRemix:
		return true
	}
	return false
}

func (provider ProviderDescriptor) SupportsOperation(operation ProviderOperation) bool {
	return slices.Contains(provider.Operations, operation)
}

func ValidateExecuteOperation(manifest Manifest, scope Scope, raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields["operation"] == nil {
		return nil
	}
	var operation ProviderOperation
	if json.Unmarshal(fields["operation"], &operation) != nil {
		return &Error{Code: "invalid_params", Message: "invalid provider operation", Outcome: "not-started"}
	}
	if operation == "" {
		return nil
	}
	var protocol string
	if json.Unmarshal(fields["protocol"], &protocol) != nil || protocol != "chat" ||
		manifest.Contract.Version.Minor < 3 || !operation.Valid() {
		return &Error{Code: "invalid_params", Message: "provider operation requires its negotiated OpenAI-compatible protocol", Outcome: "not-started"}
	}
	for _, provider := range manifest.Providers {
		if provider.ID == scope.Provider && provider.SupportsOperation(operation) &&
			slices.Contains(provider.Protocols, "chat") {
			return nil
		}
	}
	return &Error{Code: "method_not_found", Message: "provider operation is not declared by this generation", Outcome: "not-started"}
}

func validateProviderOperations(manifest Manifest, provider ProviderDescriptor) error {
	if len(provider.Operations) == 0 {
		return nil
	}
	if manifest.Contract.Version.Minor < 3 || !manifest.Contract.Has(OutboundExecutor) {
		return fmt.Errorf("provider operations require SDK API 1.3 and outbound-executor")
	}
	seen := map[ProviderOperation]bool{}
	for _, operation := range provider.Operations {
		if !operation.Valid() || seen[operation] {
			return fmt.Errorf("invalid or duplicate provider operation %q", operation)
		}
		seen[operation] = true
	}
	if !slices.Contains(provider.Protocols, "chat") {
		return fmt.Errorf("provider operations require the OpenAI-compatible chat protocol")
	}
	return nil
}

func validateExtendedModel(manifest Manifest, model Model) error {
	extended := model.Kind != "" || model.InputModalities != nil || model.OutputModalities != nil ||
		model.Released != "" || model.Temperature != nil || len(model.ServiceTiers) > 0 ||
		model.Free || model.Rate != 0 || model.RateWas != 0 || model.MaxContextWindow != 0 ||
		model.AlwaysThinks || model.DynamicTools || model.DefaultEffort != ""
	if extended && manifest.Contract.Version.Minor < 3 {
		return fmt.Errorf("extended model metadata requires SDK API 1.3")
	}
	switch model.Kind {
	case "", "chat", "image", "video", "embedding", "rerank", "speech", "transcription":
	default:
		return fmt.Errorf("unsupported model kind %q", model.Kind)
	}
	if model.MaxContextWindow < 0 || model.MaxContextWindow > 0 && model.MaxContextWindow < model.ContextWindow {
		return fmt.Errorf("maximum model context is smaller than its default window")
	}
	for _, modalities := range [][]string{model.InputModalities, model.OutputModalities} {
		seen := map[string]bool{}
		for _, modality := range modalities {
			switch modality {
			case "text", "image", "audio", "video", "pdf":
			default:
				return fmt.Errorf("unsupported model modality %q", modality)
			}
			if seen[modality] {
				return fmt.Errorf("duplicate model modality %q", modality)
			}
			seen[modality] = true
		}
	}
	if model.Released != "" {
		if _, err := time.Parse("2006-01-02", model.Released); err != nil {
			return fmt.Errorf("model release must be a YYYY-MM-DD date")
		}
	}
	if len(model.ServiceTiers) > 2 {
		return fmt.Errorf("too many model service tiers")
	}
	tiers := map[string]bool{}
	for _, tier := range model.ServiceTiers {
		if tier != "priority" && tier != "ultrafast" || tiers[tier] {
			return fmt.Errorf("invalid or duplicate model service tier %q", tier)
		}
		tiers[tier] = true
	}
	for _, rate := range []float64{model.Rate, model.RateWas} {
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			return fmt.Errorf("model subscription rates must be finite and nonnegative")
		}
	}
	if model.DefaultEffort != "" && !slices.Contains(model.Efforts, model.DefaultEffort) {
		return fmt.Errorf("default model effort must be one of its declared efforts")
	}
	return nil
}
