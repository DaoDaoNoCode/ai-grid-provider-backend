package servers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
)

const (
	// annotationModelCapabilities lists what a model can do, as a JSON array
	// of strings such as ["Chat"] or ["Vision","Chat"]. RHOAI's deploy wizard
	// writes the same key (odh-dashboard, packages/model-serving
	// modelCapabilities.ts), and MaaS reads it from the MaaSModelRef and
	// answers it as modelDetails.modelCapabilities (odh-dashboard,
	// packages/maas/bff modelref_helpers.go). The values are free text.
	annotationModelCapabilities = "opendatahub.io/model-capabilities"

	// The well-known capabilities, trimmed and lowercased, that RHOAI's
	// deploy wizard writes as "Vision", "Transcription", "Chat" and "Text
	// completion". They decide which path the ExternalModel routes to.
	capabilityVision         = "vision"
	capabilityTranscription  = "transcription"
	capabilityChat           = "chat"
	capabilityTextCompletion = "text completion"

	modelChatCompletionsPath = "/v1/chat/completions"
	modelCompletionsPath     = "/v1/completions"
)

// modelGitAnnotations are the prefixed annotations a Model may carry in git
// mode: the console's display name and description, and its capabilities,
// which the MaaSModelRef carries to MaaS.
var modelGitAnnotations = gitAnnotations{
	annotationDisplayName:       nil,
	annotationDescription:       nil,
	annotationModelCapabilities: func(value string) error { _, err := parseModelCapabilities(value); return err },
}

// parseModelCapabilities reads the capabilities annotation's value: a JSON
// array of strings, none blank. The strings are returned as written.
func parseModelCapabilities(value string) ([]string, error) {
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return nil, fmt.Errorf(`must be a JSON array of strings, such as ["Chat"]: %w`, err)
	}
	items, ok := parsed.([]any)
	if !ok {
		return nil, errors.New(`must be a JSON array of strings, such as ["Chat"]`)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		capability, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("item %d is not a string", i)
		}
		if strings.TrimSpace(capability) == "" {
			return nil, fmt.Errorf("item %d is empty", i)
		}
		out = append(out, capability)
	}
	return out, nil
}

// normalizeModelCapability trims and lowercases a capability, so "Text
// completion" and " text COMPLETION " compare alike. Nothing else is folded:
// "text_completion" is another capability.
func normalizeModelCapability(capability string) string {
	return strings.ToLower(strings.TrimSpace(capability))
}

// modelCompletionOnly reports whether the model is completion-only: "Text
// completion" is the only well-known capability it has, so it has none of
// "Chat", "Vision" and "Transcription". Other values don't count. A model
// without the annotation, or with one that doesn't parse, is a chat model.
func modelCompletionOnly(object *aigridv1.Model) bool {
	value, ok := object.GetMetadata().GetAnnotations()[annotationModelCapabilities]
	if !ok {
		return false
	}
	capabilities, err := parseModelCapabilities(value)
	if err != nil {
		return false
	}
	completion, other := false, false
	for _, capability := range capabilities {
		switch normalizeModelCapability(capability) {
		case capabilityTextCompletion:
			completion = true
		case capabilityChat, capabilityVision, capabilityTranscription:
			other = true
		}
	}
	return completion && !other
}

// modelExternalPath is the path the model's ExternalModel routes to. When
// the tenant's MaaSTenantConfig enables API translation
// (spec.apiTranslation.enabled), as it does on AI Grid's ingress clusters,
// MaaS sends every request for an ExternalModel to its provider on the
// ExternalModel's path, whatever path the client called: a chat request to a
// model whose path is /v1/completions arrives as a completions request. So a
// completion-only model gets /v1/completions, and every other model
// /v1/chat/completions. With translation off, MaaS keeps the client's path
// and this path has no effect.
func modelExternalPath(object *aigridv1.Model) string {
	if modelCompletionOnly(object) {
		return modelCompletionsPath
	}
	return modelChatCompletionsPath
}
