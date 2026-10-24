package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// ModelEntry mirrors the metadata the litellm proxy exposes per configured model.
// The proxy reads `model_info` blocks from litellm_config.yaml and surfaces them
// on /v1/models. Anything not in the config simply remains a zero value.
//
// knownModelContextWindows is a built-in fallback table of context-window
// sizes (in tokens) for well-known model families. It is used only when the
// litellm proxy does not declare a `context_window` in model_info for the
// model. The proxy-declared value always wins over this table. Keys are
// matched as prefixes (case-insensitive, ignoring ":suffix" variants), so
// "strata-swift" also covers "strata-swift:foo" and "deepseek-v4-flash-0731"
// covers "deepseek-v4-flash-0731:all".
//
// Values are the model's advertised maximum context window. These are static
// best-effort numbers; operators can override per-model by adding
// `model_info.context_window` to litellm_config.yaml.
var knownModelContextWindows = map[string]int{
	"gpt-oss-20b":        32768,
	"gpt-oss-120b":       65536,
	"gpt-4o":             128000,
	"gpt-4o-mini":        128000,
	"gpt-4.1":            1047576,
	"o1":                 200000,
	"o3":                 200000,
	"claude-3-5-sonnet":  200000,
	"claude-3-7-sonnet":  200000,
	"claude-sonnet-4":    200000,
	"claude-opus-4":      200000,
	"deepseek-v4":        131072,
	"deepseek-v3":        65536,
	"deepseek-chat":      65536,
	"deepseek-reasoner":  65536,
	"glm-5":              131072,
	"glm-4":              128000,
	"kimi-k2":            131072,
	"kimi-k3":            262144,
	"gemini-3":           1048576,
	"gemini-2.5":         1048576,
	"gemini-2.0":         1048576,
	"gemini-1.5":         1048576,
	"llama-3.3":          131072,
	"llama-3.1":          131072,
	"mistral-large":      128000,
	"qwen-2.5":           131072,
	"qwen-3":             131072,
	"strata-swift":       65536,
	"ling-3.0-flash":     32768,
}

type ModelEntry struct {
	Name                    string
	Mode                    string // chat | image_generation | image_edit | audio_speech | audio_transcription | embedding
	SupportsVision          bool
	SupportsFunctionCalling bool
// MaxContextTokens is the model's context window in tokens, as declared
	// in litellm_config.yaml model_info.context_window and surfaced by the
	// proxy on /v1/models. Zero means "unknown" — callers should fall back
	// to the configured eco trigger rather than assume a window.
	MaxContextTokens int
}

// ModelRegistry caches the list of models the litellm proxy knows about.
// It's the single source of truth for what models the server exposes and
// what they support. All capability gates in the codebase read from here.
type ModelRegistry struct {
	mu     sync.RWMutex
	byName map[string]ModelEntry
	list   []ModelEntry
}

// Models is the package-level singleton, populated at startup by InitModels.
var Models = &ModelRegistry{byName: map[string]ModelEntry{}}

type litellmModelEntry struct {
	ID                      string `json:"id"`
	Mode                    string `json:"mode"`
	SupportsVision          bool   `json:"supports_vision"`
	SupportsFunctionCalling bool   `json:"supports_function_calling"`
MaxContextTokens        int    `json:"context_window"`
}

type litellmModelList struct {
	Data []litellmModelEntry `json:"data"`
}

// InitModels fetches the model list from the litellm proxy. Call once after
// InitLiteLLM() succeeds. Safe to call again to refresh.
func InitModels(ctx context.Context) error {
	return Models.Refresh(ctx)
}

// Refresh GETs /v1/models from the litellm proxy and atomically swaps the
// cached registry. On error the previous cache is preserved.
func (r *ModelRegistry) Refresh(ctx context.Context) error {
	url := LiteLLMBaseURL + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching litellm /v1/models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("litellm /v1/models returned %d", resp.StatusCode)
	}

	var parsed litellmModelList
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return fmt.Errorf("decoding litellm /v1/models: %w", err)
	}

	byName := make(map[string]ModelEntry, len(parsed.Data))
	list := make([]ModelEntry, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		entry := ModelEntry{
			Name:                    m.ID,
			Mode:                    m.Mode,
			SupportsVision:          m.SupportsVision,
			SupportsFunctionCalling: m.SupportsFunctionCalling,
MaxContextTokens:        m.MaxContextTokens,
		}
		byName[m.ID] = entry
		list = append(list, entry)
	}

	r.mu.Lock()
	r.byName = byName
	r.list = list
	r.mu.Unlock()

	utils.Log("[Models] Loaded %d models from litellm proxy", len(list))
	return nil
}

// Get returns the entry for a model name and whether it's known.
func (r *ModelRegistry) Get(name string) (ModelEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byName[name]
	return entry, ok
}

// All returns a snapshot of every known model entry.
func (r *ModelRegistry) All() []ModelEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelEntry, len(r.list))
	copy(out, r.list)
	return out
}

// IsKnown reports whether the model exists in the registry.
func (r *ModelRegistry) IsKnown(name string) bool {
	_, ok := r.Get(name)
	return ok
}

// defaultContextWindow is used when the registry has no entry for a model
// (e.g. before Refresh, or a custom/unknown name). 128k is a safe middle
// ground: it protects the small local models (65k) without starving the
// large ones (1M) on the shared budget path.
const defaultContextWindow = 131072

// MaxContextTokensForPrefixed is a small helper used by the eco tail
// capping: it returns the model's declared context window if known, or a
// safe default (defaultContextWindow) otherwise. Unlike MaxContextTokensFor
// (which returns 0 for unknown — right for the eco *trigger* fallback), the
// tail budget must ALWAYS pick a concrete bound so a model with no declared
// window still gets a bounded tail instead of the old unbounded 100k.
func (r *ModelRegistry) MaxContextTokensForPrefixed(name string) int {
	if w := r.MaxContextTokensFor(name); w > 0 {
		return w
	}
	return defaultContextWindow
}

// IsImageGenModel reports whether the model produces images.

// IsActionModel reports whether the model is a chat model that supports tool calls.
// This is the gate for sending `tools` to the LLM and injecting skill prompts.
func (r *ModelRegistry) IsActionModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return entry.Mode == "chat" && entry.SupportsFunctionCalling
}

// IsVisionModel reports whether the model can ingest images.
func (r *ModelRegistry) IsVisionModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return entry.SupportsVision
}

// IsImageGenModel reports whether the model produces images.
func (r *ModelRegistry) IsImageGenModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return entry.Mode == "image_generation"
}

// IsImageEditModel reports whether the model edits an input image.
func (r *ModelRegistry) IsImageEditModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return entry.Mode == "image_edit"
}

// IsAudioModel reports whether the model is an audio (TTS or STT) model.
func (r *ModelRegistry) IsAudioModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return strings.HasPrefix(entry.Mode, "audio_")
}

// MaxContextTokensFor returns the declared context window (in tokens) for the
// model with the given registry name. It checks the exact name first, then a
// prefix match (e.g. "strata-swift" when the registry has "strata-swift").
// Returns 0 when the model is unknown or no window was declared — callers use
// the configured eco fallback in that case.
func (r *ModelRegistry) MaxContextTokensFor(name string) int {
	if name == "" {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.byName[name]; ok {
		return entry.MaxContextTokens
	}
	// Prefix fallback: registry entries may carry a ":suffix" variant
	// (e.g. "deepseek-v4-flash-0731:all") while the conversation selects the
	// bare name (or vice versa). Match on the part before ":suffix".
	base := name
	if i := strings.IndexByte(name, ':'); i >= 0 {
		base = name[:i]
	}
	for n, entry := range r.byName {
		nbase := n
		if i := strings.IndexByte(n, ':'); i >= 0 {
			nbase = n[:i]
		}
		if nbase == base && entry.MaxContextTokens > 0 {
			return entry.MaxContextTokens
		}
	}
	// No proxy-declared window: fall back to the built-in table so the
	// dynamic trigger works even before an operator adds context_window to
	// litellm_config.yaml.
	return builtinContextWindow(base)
}

// builtinContextWindow looks up a model family in knownModelContextWindows by
// exact key, then by longest-prefix match (so "strata-swift:foo" resolves to
// "strata-swift"). Returns 0 when nothing matches.
func builtinContextWindow(base string) int {
	if base == "" {
		return 0
	}
	b := strings.ToLower(base)
	if v, ok := knownModelContextWindows[b]; ok {
		return v
	}
	// Longest-prefix match: trim the key from its most specific form down
	// through '-' boundaries so "deepseek-v4-flash-0731" resolves to
	// "deepseek-v4" and "strata-swift-1.5" resolves to "strata-swift". The
	// first (longest) match wins.
	for {
		if v, ok := knownModelContextWindows[b]; ok {
			return v
		}
		i := strings.LastIndexByte(b, '-')
		if i <= 0 {
			break
		}
		b = b[:i]
	}
	return 0
}
