package foundry

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type Identity struct {
	ResourceID   string
	TenantID     string
	ClientID     string
	ClientSecret string `json:"-"`
}

type Snapshot struct {
	ResourceID          string       `json:"resource_id"`
	Endpoint            string       `json:"endpoint"`
	Deployments         []Deployment `json:"deployments"`
	RefreshedAt         time.Time    `json:"refreshed_at"`
	ImageCatalogChecked bool         `json:"image_catalog_checked,omitempty"`
	ImageCatalogError   string       `json:"image_catalog_error,omitempty"`
}

const ModelsAPISource = "models-api"

type Deployment struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	ModelName         string            `json:"model_name"`
	ModelVersion      string            `json:"model_version"`
	ModelFormat       string            `json:"model_format"`
	ProvisioningState string            `json:"provisioning_state"`
	SKU               string            `json:"sku"`
	Capabilities      map[string]string `json:"capabilities"`
	Source            string            `json:"source,omitempty"`
}

type Operation string

const (
	Chat       Operation = "chat"
	Embeddings Operation = "embeddings"
	Images     Operation = "images"
	ImageEdits Operation = "image_edits"
	Vision     Operation = "vision"
)

func (s Snapshot) Find(name string) (Deployment, bool) {
	for _, deployment := range s.Deployments {
		if deployment.Name == name {
			return deployment, true
		}
	}
	return Deployment{}, false
}

func (s Snapshot) Names(op Operation) []string {
	names := make([]string, 0, len(s.Deployments))
	for _, deployment := range s.Deployments {
		if deployment.Supports(op) {
			names = append(names, deployment.Name)
		}
	}
	sort.Strings(names)
	return names
}

func (d Deployment) Supports(op Operation) bool {
	return d.UnsupportedReason(op) == ""
}

func (d Deployment) UnsupportedReason(op Operation) string {
	bit, ok := operationBits[op]
	if !ok {
		return "operation is not implemented by the OpenAI v1 integration"
	}
	if d.Source == ModelsAPISource {
		if op != Images && op != ImageEdits {
			return "API catalog entries are only used for supported image operations"
		}
	} else if !strings.EqualFold(d.ProvisioningState, "Succeeded") {
		return "deployment provisioning has not succeeded"
	}
	if strings.Contains(strings.ToLower(d.SKU), "batch") {
		return "batch deployments do not support synchronous inference"
	}
	if present, enabled, valid := d.capability("batchonly"); present && (!valid || enabled) {
		return "deployment is batch-only or has an unrecognized batch-only capability"
	}
	for key, value := range d.Capabilities {
		switch capabilityKey(key) {
		case "inferenceprotocol", "protocol":
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "openai", "openai-compatible", "openai_v1", "openai-v1":
			default:
				return "deployment requires an unsupported or unrecognized inference protocol"
			}
		}
	}
	model := canonicalModel(d.ModelName)
	format := capabilityKey(d.ModelFormat)
	if format == "anthropic" || strings.HasPrefix(model, "claude-") {
		return "Anthropic-native messages are not supported by the OpenAI v1 integration"
	}
	if model == "dall-e-3" {
		return "DALL-E request and response options are not implemented by the image client"
	}
	if format == "cohere" && strings.Contains(model, "embed") {
		return "Cohere embeddings require a model-specific embedding adapter"
	}
	if (strings.HasPrefix(model, "gpt-") && (strings.HasSuffix(model, "-pro") || strings.Contains(model, "-codex"))) ||
		strings.HasPrefix(model, "codex-") || model == "computer-use-preview" {
		return "this model requires the Responses API, which is not implemented"
	}
	if model == "gpt-35-turbo-instruct" || model == "gpt-3.5-turbo-instruct" {
		return "legacy text completions are not implemented by the chat integration"
	}
	for _, excluded := range []string{"realtime", "audio", "transcribe", "whisper", "tts", "sora", "batch"} {
		for _, token := range strings.FieldsFunc(model+"-"+strings.ToLower(d.ModelVersion), func(r rune) bool { return r == '-' || r == '_' }) {
			if token == excluded {
				return "realtime, audio, video, and batch-only model APIs are not implemented"
			}
		}
	}
	profile, known := modelProfiles[model]
	if !known {
		profile, known = d.advertisedChatProfile(format)
	}
	if !known || !matchesFormat(format, profile.format) {
		return fmt.Sprintf("model %q (format %q) has neither a known profile nor supported chat capability metadata", d.ModelName, d.ModelFormat)
	}
	supported := profile.operations
	if model == "gpt-4" {
		switch strings.ToLower(d.ModelVersion) {
		case "vision-preview", "turbo-2024-04-09":
			supported |= visionBit
		}
		if _, enabled, valid := d.capability(capabilityKeys[Vision]...); valid && enabled {
			supported |= visionBit
		}
	}
	if model == "model-router" {
		if _, enabled, valid := d.capability(capabilityKeys[Vision]...); valid && enabled {
			supported |= visionBit
		}
	}
	if supported&bit == 0 {
		return fmt.Sprintf("canonical model metadata does not identify a supported %s model", op)
	}
	if present, enabled, valid := d.capability(capabilityKeys[op]...); present {
		if !valid {
			return fmt.Sprintf("deployment has an unrecognized %s capability value", op)
		}
		if !enabled {
			return fmt.Sprintf("deployment metadata explicitly disables %s", op)
		}
	}
	if op == Vision && !d.Supports(Chat) {
		return "vision requires a supported chat-completions deployment"
	}
	return ""
}

// New model names can advertise chat support on a format already implemented
// here. Protocol and model-kind exclusions are checked before this fallback.
func (d Deployment) advertisedChatProfile(format string) (modelProfile, bool) {
	_, enabled, valid := d.capability(capabilityKeys[Chat]...)
	if !enabled || !valid {
		return modelProfile{}, false
	}
	for _, profile := range modelProfiles {
		if profile.operations&chatBit == 0 || !matchesFormat(format, profile.format) {
			continue
		}
		operations := chatBit
		if _, enabled, valid := d.capability(capabilityKeys[Vision]...); enabled && valid {
			operations |= visionBit
		}
		return modelProfile{format: format, operations: operations}, true
	}
	return modelProfile{}, false
}

// ARM capability keys are extensible. Only recognized boolean hints are used;
// explicit restrictions still override the model profiles.
var capabilityKeys = map[Operation][]string{
	Chat:       {"chatcompletion", "chatcompletions"},
	Embeddings: {"embeddings", "embedding"},
	Images:     {"imagegeneration"},
	ImageEdits: {"imageedits", "imageediting"},
	Vision:     {"vision", "imageinput"},
}

func capabilityKey(value string) string {
	return strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(value)))
}

func (d Deployment) capability(keys ...string) (present, enabled, valid bool) {
	enabled, valid = true, true
	for key, value := range d.Capabilities {
		for _, recognized := range keys {
			if capabilityKey(key) != recognized {
				continue
			}
			present = true
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "true":
			case "false":
				enabled = false
			default:
				valid, enabled = false, false
			}
		}
	}
	return present, present && enabled, present && valid
}

func canonicalModel(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if len(name) > 11 && name[len(name)-11] == '-' {
		if _, err := time.Parse("2006-01-02", name[len(name)-10:]); err == nil {
			return name[:len(name)-11]
		}
	}
	return name
}

func matchesFormat(actual, expected string) bool {
	if actual == expected {
		return true
	}
	return expected == "mistral" && actual == "mistralai"
}

type operationSet uint8

const (
	chatBit operationSet = 1 << iota
	embeddingsBit
	imagesBit
	imageEditsBit
	visionBit
)

var operationBits = map[Operation]operationSet{
	Chat: chatBit, Embeddings: embeddingsBit, Images: imagesBit, ImageEdits: imageEditsBit, Vision: visionBit,
}

type modelProfile struct {
	format     string
	operations operationSet
}

// Profiles provide defaults when ARM omits capability metadata. They are keyed
// by canonical model names, never customer deployment aliases.
var modelProfiles = map[string]modelProfile{
	"model-router":                           {"openai", chatBit},
	"gpt-35-turbo":                           {"openai", chatBit},
	"gpt-3.5-turbo":                          {"openai", chatBit},
	"gpt-4":                                  {"openai", chatBit},
	"gpt-4-32k":                              {"openai", chatBit},
	"gpt-4-turbo":                            {"openai", chatBit | visionBit},
	"gpt-4-turbo-preview":                    {"openai", chatBit},
	"gpt-4o":                                 {"openai", chatBit | visionBit},
	"gpt-4o-mini":                            {"openai", chatBit | visionBit},
	"gpt-4.1":                                {"openai", chatBit | visionBit},
	"gpt-4.1-mini":                           {"openai", chatBit | visionBit},
	"gpt-4.1-nano":                           {"openai", chatBit | visionBit},
	"gpt-4.5-preview":                        {"openai", chatBit | visionBit},
	"gpt-5":                                  {"openai", chatBit | visionBit},
	"gpt-5-mini":                             {"openai", chatBit | visionBit},
	"gpt-5-nano":                             {"openai", chatBit | visionBit},
	"gpt-5-chat":                             {"openai", chatBit | visionBit},
	"gpt-5-chat-latest":                      {"openai", chatBit | visionBit},
	"gpt-5.1":                                {"openai", chatBit | visionBit},
	"gpt-5.1-chat":                           {"openai", chatBit | visionBit},
	"gpt-5.2":                                {"openai", chatBit | visionBit},
	"gpt-5.2-chat":                           {"openai", chatBit | visionBit},
	"gpt-5.4":                                {"openai", chatBit | visionBit},
	"gpt-5.4-mini":                           {"openai", chatBit | visionBit},
	"gpt-5.4-nano":                           {"openai", chatBit | visionBit},
	"gpt-5.5":                                {"openai", chatBit | visionBit},
	"gpt-5.6-sol":                            {"openai", chatBit | visionBit},
	"gpt-5.6-terra":                          {"openai", chatBit | visionBit},
	"gpt-5.6-luna":                           {"openai", chatBit | visionBit},
	"gpt-6-astra":                            {"openai", chatBit | visionBit},
	"gpt-chat-latest":                        {"openai", chatBit},
	"o1":                                     {"openai", chatBit | visionBit},
	"o1-preview":                             {"openai", chatBit},
	"o1-mini":                                {"openai", chatBit},
	"o3":                                     {"openai", chatBit | visionBit},
	"o3-mini":                                {"openai", chatBit},
	"o4-mini":                                {"openai", chatBit | visionBit},
	"text-embedding-ada-002":                 {"openai", embeddingsBit},
	"text-embedding-3-small":                 {"openai", embeddingsBit},
	"text-embedding-3-large":                 {"openai", embeddingsBit},
	"gpt-image-1":                            {"openai", imagesBit | imageEditsBit},
	"gpt-image-1-mini":                       {"openai", imagesBit | imageEditsBit},
	"gpt-image-1.5":                          {"openai", imagesBit | imageEditsBit},
	"gpt-image-2":                            {"openai", imagesBit | imageEditsBit},
	"deepseek-r1":                            {"deepseek", chatBit},
	"deepseek-r1-0528":                       {"deepseek", chatBit},
	"deepseek-v3":                            {"deepseek", chatBit},
	"deepseek-v3-0324":                       {"deepseek", chatBit},
	"deepseek-v3.1":                          {"deepseek", chatBit},
	"deepseek-v3.2":                          {"deepseek", chatBit},
	"phi-4":                                  {"microsoft", chatBit},
	"phi-4-mini-instruct":                    {"microsoft", chatBit},
	"phi-4-reasoning":                        {"microsoft", chatBit},
	"phi-4-mini-reasoning":                   {"microsoft", chatBit},
	"phi-4-multimodal-instruct":              {"microsoft", chatBit | visionBit},
	"phi-3.5-mini-instruct":                  {"microsoft", chatBit},
	"phi-3.5-moe-instruct":                   {"microsoft", chatBit},
	"phi-3.5-vision-instruct":                {"microsoft", chatBit | visionBit},
	"meta-llama-3.1-8b-instruct":             {"meta", chatBit},
	"meta-llama-3.1-70b-instruct":            {"meta", chatBit},
	"meta-llama-3.1-405b-instruct":           {"meta", chatBit},
	"llama-3.3-70b-instruct":                 {"meta", chatBit},
	"llama-3.2-11b-vision-instruct":          {"meta", chatBit | visionBit},
	"llama-3.2-90b-vision-instruct":          {"meta", chatBit | visionBit},
	"llama-4-scout-17b-16e-instruct":         {"meta", chatBit | visionBit},
	"llama-4-maverick-17b-128e-instruct-fp8": {"meta", chatBit | visionBit},
	"mistral-large":                          {"mistral", chatBit},
	"mistral-large-2407":                     {"mistral", chatBit},
	"mistral-large-2411":                     {"mistral", chatBit},
	"mistral-small":                          {"mistral", chatBit},
	"mistral-small-2503":                     {"mistral", chatBit | visionBit},
	"mistral-medium-2505":                    {"mistral", chatBit | visionBit},
	"mistral-nemo":                           {"mistral", chatBit},
	"codestral-2501":                         {"mistral", chatBit},
	"pixtral-large-2411":                     {"mistral", chatBit | visionBit},
	"command-r":                              {"cohere", chatBit},
	"command-r-plus":                         {"cohere", chatBit},
	"command-r-08-2024":                      {"cohere", chatBit},
	"command-r-plus-08-2024":                 {"cohere", chatBit},
	"command-a-03-2025":                      {"cohere", chatBit},
	"cohere-command-r":                       {"cohere", chatBit},
	"cohere-command-r-plus":                  {"cohere", chatBit},
	"cohere-command-a":                       {"cohere", chatBit},
	"grok-3":                                 {"xai", chatBit},
	"grok-3-mini":                            {"xai", chatBit},
	"grok-4":                                 {"xai", chatBit},
	"grok-4.3":                               {"xai", chatBit},
	"grok-4-fast-reasoning":                  {"xai", chatBit | visionBit},
	"grok-4-fast-non-reasoning":              {"xai", chatBit | visionBit},
}
