package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/azukaar/plurality/src/utils"
)

// OpenIDConfig configures login via an external OpenID Connect provider. The
// client (Flutter web/native) drives the OAuth flow itself using PKCE, so no
// client secret or redirect URL is needed server-side — the server only
// verifies the resulting ID token.
type OpenIDConfig struct {
	Enabled bool `json:"enabled"`
	// Name is the human-friendly provider label shown in the UI (e.g.
	// "Login with Authentik"). Defaults to "OpenID" when empty.
	Name string `json:"name"`
	// BtnColor / BtnBg1 / BtnBg2 style the login button in the UI. All are hex
	// strings (e.g. "#FFFFFF"); empty falls back to the app theme's secondary
	// colours. BtnColor is the foreground (text) colour. BtnBg1 is the
	// background; when BtnBg2 is also set the background is a linear gradient
	// from BtnBg1 to BtnBg2, otherwise BtnBg1 is a plain fill.
	BtnColor  string   `json:"btn_color"`
	BtnBg1    string   `json:"btn_bg1"`
	BtnBg2    string   `json:"btn_bg2"`
	Issuer    string   `json:"issuer"`
	ClientID  string   `json:"client_id"`
	Allowlist []string `json:"allowlist"`
}

// ShortcutModel is one model entry inside a shortcut. Tools maps tool keys
// (e.g. "search_web") to their default state ("true" / "ask" / "false").
type ShortcutModel struct {
	Name  string            `json:"name"`
	Tools map[string]string `json:"tools,omitempty"`
}

// ShortcutModels groups the per-mode model selections for a shortcut.
type ShortcutModels struct {
	Text      *ShortcutModel `json:"text,omitempty"`
	Vision    *ShortcutModel `json:"vision,omitempty"`
	ImageGen  *ShortcutModel `json:"imagegen,omitempty"`
	ImageEdit *ShortcutModel `json:"imageedit,omitempty"`
}

// Shortcut is a named bundle of model selections + tool defaults shown in the
// picker. The names "fast", "medium", and "smart" are reserved and surfaced in
// the UI; any extra entry is preserved on disk but ignored by the picker.
type Shortcut struct {
	Name    string         `json:"name"`
	Label   string         `json:"label"`
	Pricing string         `json:"pricing"`
	Color   string         `json:"color"`
	Models  ShortcutModels `json:"models"`
}

// WebhookConfig holds rate-limit knobs for the public webhook trigger
// endpoint. Both limits are evaluated per source IP (honouring
// X-Forwarded-For); zero/negative means "use the default".
type WebhookConfig struct {
	// PerClientPerMinute caps requests from one IP across ALL webhooks in
	// 60s. Crossing this trips a permanent in-memory block until restart.
	// Default: 200.
	PerClientPerMinute int `json:"per_client_per_minute"`
	// PerWebhookPerMinute caps requests from one IP to one webhook ID in
	// 60s. Exceeding returns 429 with Retry-After. Default: 10.
	PerWebhookPerMinute int `json:"per_webhook_per_minute"`
}

// NotificationsConfig configures the optional NTFY-based "send_notification"
// tool. When NtfyURL and Topic are both set, the tool is force-included in
// every LLM tool list; otherwise it's hidden entirely.
type NotificationsConfig struct {
	NtfyURL string `json:"ntfy_url"`
	Topic   string `json:"topic"`
	Token   string `json:"token"`
}

// EcoConfig controls the server-side "eco mode" rolling-checkpoint
// compaction. Both values are in tokens (as reported by LiteLLM in the last
// assistant message's prompt_tokens). When the previous prompt crosses
// TriggerTokens, the oldest turns are summarised into a single checkpoint so
// the remaining live tail is roughly TargetTokens wide.
//
// ContextFraction makes the trigger dynamic per model: when a model declares
// a context window (litellm_config.yaml model_info.context_window) the
// effective trigger is min(TriggerTokens, round(ContextFraction * window)).
// So a 64k model with ContextFraction 0.8 compacts at 51,200 tokens instead
// of waiting forever for the old static 100k floor that a small window could
// never reach. ContextFraction <= 0 disables the dynamic part (static
// trigger only). If the model window is unknown, the static TriggerTokens is
// used unchanged.
type EcoConfig struct {
	TriggerTokens   int     `json:"trigger_tokens"`
	TargetTokens    int     `json:"target_tokens"`
	ContextFraction float64 `json:"context_fraction"`
}

type Config struct {
	JWTSecret     string              `json:"jwt_secret"`
	OpenID        OpenIDConfig        `json:"openid"`
	Shortcuts     []Shortcut          `json:"shortcuts"`
	Webhook       WebhookConfig       `json:"webhook"`
	Notifications NotificationsConfig `json:"notifications"`
	Eco           EcoConfig           `json:"eco"`
}

const (
	defaultPerClientPerMinute  = 200
	defaultPerWebhookPerMinute = 10
	defaultEcoTriggerTokens    = 100000
	defaultEcoTargetTokens     = 50000
	defaultEcoContextFraction  = 0.8
)

// ReservedShortcutNames are the picker-visible shortcut names, in display order.
var ReservedShortcutNames = []string{"fast", "medium", "smart"}

var (
	cfgMu sync.RWMutex
	cfg   Config
)

// dataDir mirrors skills.dataDir / mcp.dataDir.
func dataDir() string {
	if p := os.Getenv("DATA_DIR"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "data")
}

func configPath() string {
	return filepath.Join(dataDir(), "config.json")
}

// LoadConfig reads data/config.json (creating defaults if missing) and
// applies env-var overrides on top. Env wins over file.
func LoadConfig() error {
	cfgMu.Lock()
	defer cfgMu.Unlock()

	if err := os.MkdirAll(dataDir(), 0755); err != nil {
		return err
	}

	path := configPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg = Config{
			JWTSecret: randomHex(32),
			OpenID: OpenIDConfig{
				Enabled:   false,
				Allowlist: []string{},
			},
			Webhook: WebhookConfig{
				PerClientPerMinute:  defaultPerClientPerMinute,
				PerWebhookPerMinute: defaultPerWebhookPerMinute,
			},
			Eco: EcoConfig{
				TriggerTokens:   defaultEcoTriggerTokens,
				TargetTokens:    defaultEcoTargetTokens,
				ContextFraction: defaultEcoContextFraction,
			},
		}
		validateShortcuts(&cfg)
		if err := writeConfigLocked(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		var loaded Config
		if err := json.Unmarshal(data, &loaded); err != nil {
			return err
		}
		needsWrite := false
		if loaded.JWTSecret == "" {
			loaded.JWTSecret = randomHex(32)
			needsWrite = true
		}
		if validateShortcuts(&loaded) {
			needsWrite = true
		}
		if loaded.Webhook.PerClientPerMinute <= 0 {
			loaded.Webhook.PerClientPerMinute = defaultPerClientPerMinute
			needsWrite = true
		}
		if loaded.Webhook.PerWebhookPerMinute <= 0 {
			loaded.Webhook.PerWebhookPerMinute = defaultPerWebhookPerMinute
			needsWrite = true
		}
		if loaded.Eco.TriggerTokens <= 0 {
			loaded.Eco.TriggerTokens = defaultEcoTriggerTokens
			needsWrite = true
		}
		if loaded.Eco.TargetTokens <= 0 {
			loaded.Eco.TargetTokens = defaultEcoTargetTokens
			needsWrite = true
		}
		if loaded.Eco.ContextFraction <= 0 {
			loaded.Eco.ContextFraction = defaultEcoContextFraction
			needsWrite = true
		}
		cfg = loaded
		if needsWrite {
			if err := writeConfigLocked(); err != nil {
				return err
			}
		}
	}

	applyEnvOverrides()
	return nil
}

func writeConfigLocked() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

func applyEnvOverrides() {
	if v := os.Getenv("JWT_SECRET"); v != "" {
		cfg.JWTSecret = v
	}
	if v := os.Getenv("OPENID_ISSUER"); v != "" {
		cfg.OpenID.Issuer = v
		cfg.OpenID.Enabled = true
	}
	if v := os.Getenv("OPENID_NAME"); v != "" {
		cfg.OpenID.Name = v
	}
	if v := os.Getenv("OPENID_BTN_COLOR"); v != "" {
		cfg.OpenID.BtnColor = v
	}
	if v := os.Getenv("OPENID_BTN_BG1"); v != "" {
		cfg.OpenID.BtnBg1 = v
	}
	if v := os.Getenv("OPENID_BTN_BG2"); v != "" {
		cfg.OpenID.BtnBg2 = v
	}
	if v := os.Getenv("OPENID_CLIENT_ID"); v != "" {
		cfg.OpenID.ClientID = v
	}
	if v := os.Getenv("OPENID_ALLOWLIST"); v != "" {
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		cfg.OpenID.Allowlist = out
	}
	if v := os.Getenv("NTFY_URL"); v != "" {
		cfg.Notifications.NtfyURL = v
	}
	if v := os.Getenv("NTFY_TOPIC"); v != "" {
		cfg.Notifications.Topic = v
	}
	if v := os.Getenv("NTFY_TOKEN"); v != "" {
		cfg.Notifications.Token = v
	}
}

func NotificationsEnabled() bool {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg.Notifications.NtfyURL != "" && cfg.Notifications.Topic != ""
}

func GetConfig() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

// SetShortcut overwrites the entry with matching name in cfg.Shortcuts and
// persists data/config.json. The name must be one of ReservedShortcutNames.
func SetShortcut(s Shortcut) error {
	name := strings.ToLower(strings.TrimSpace(s.Name))
	reserved := false
	for _, r := range ReservedShortcutNames {
		if name == r {
			reserved = true
			break
		}
	}
	if !reserved {
		return errors.New("unknown shortcut: " + name)
	}
	s.Name = name

	cfgMu.Lock()
	defer cfgMu.Unlock()

	replaced := false
	for i := range cfg.Shortcuts {
		if cfg.Shortcuts[i].Name == name {
			cfg.Shortcuts[i] = s
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Shortcuts = append(cfg.Shortcuts, s)
		validateShortcuts(&cfg)
	}
	return writeConfigLocked()
}


func OpenIDEnabled() bool {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg.OpenID.Enabled && cfg.OpenID.Issuer != "" && cfg.OpenID.ClientID != ""
}

// AllowlistMatch returns true if the email or any of the given names (e.g.
// username/nickname) matches an allowlist entry. The allowlist is OPTIONAL:
// when it contains no usable entries, every authenticated user is allowed.
// Otherwise "*" allows any authenticated user; "*@domain" matches that email
// domain; any other entry must match the email or one of the names exactly
// (case-insensitive).
func AllowlistMatch(email string, names ...string) bool {
	cfgMu.RLock()
	defer cfgMu.RUnlock()

	email = strings.ToLower(strings.TrimSpace(email))

	// Normalise the candidate names, dropping empties.
	normNames := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" {
			normNames = append(normNames, n)
		}
	}

	// Optional whitelist: no usable entries => allow everyone.
	hasEntry := false
	for _, entry := range cfg.OpenID.Allowlist {
		if strings.TrimSpace(entry) != "" {
			hasEntry = true
			break
		}
	}
	if !hasEntry {
		return true
	}

	if email == "" && len(normNames) == 0 {
		return false
	}
	for _, entry := range cfg.OpenID.Allowlist {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if strings.HasPrefix(entry, "*@") {
			if email != "" && strings.HasSuffix(email, entry[1:]) {
				return true
			}
			continue
		}
		if email != "" && entry == email {
			return true
		}
		for _, n := range normNames {
			if entry == n {
				return true
			}
		}
	}
	return false
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Entropy failure must not silently weaken secrets (e.g. the JWT secret).
		utils.Fatal("[Auth] randomHex: failed to read from crypto/rand", err)
	}
	return hex.EncodeToString(b)
}

// defaultShortcutFastTools is the slim on-by-default tool set for the "fast" shortcut.
var defaultShortcutFastTools = map[string]string{
	"conversations__retrieve_conversation": "true",
	"conversations__search_conversations":  "true",
	"generate_image":                       "true",
	"place_search":                         "true",
	"search_web":                           "true",
	"visit_link":                           "true",
	"update_important_memory":              "true",
}

// defaultShortcutMediumTools is the on-by-default tool set for the "medium" shortcut.
var defaultShortcutMediumTools = map[string]string{
	"search_web":              "true",
	"place_search":            "true",
	"visit_link":              "true",
	"generate_image":          "true",
	"long_task":               "true",
	"update_important_memory": "true",
}

// defaultShortcutSmartTools is the full on-by-default tool set for the "smart" shortcut.
var defaultShortcutSmartTools = map[string]string{
	"search_web":                                          "true",
	"place_search":                                        "true",
	"visit_link":                                          "true",
	"generate_image":                                      "true",
	"long_task":                                           "true",
	"manage_cron":                                         "true",
	"manage_webhook":                                      "true",
	"list_presets":                                        "true",
	"update_important_memory":                             "true",
	"conversations__search_conversations":                 "true",
	"conversations__retrieve_conversation":                "true",
	"conversations__create_conversation":                  "true",
	"conversations__parallel_sub_agent":                   "true",
	"conversations__parallel_sub_agent_background_manage": "true",
}

// defaultShortcuts returns baked-in definitions for the three reserved names.
func defaultShortcuts() map[string]Shortcut {
	cloneTools := func(src map[string]string) map[string]string {
		out := make(map[string]string, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}

	return map[string]Shortcut{
		"fast": {
			Name: "fast", Label: "Fast and low cost", Pricing: "$", Color: "green",
			Models: ShortcutModels{
				Text:      &ShortcutModel{Name: "gpt-oss-20b", Tools: cloneTools(defaultShortcutFastTools)},
				Vision:    &ShortcutModel{Name: "gemini-3.1-flash-lite", Tools: cloneTools(defaultShortcutFastTools)},
				ImageGen:  &ShortcutModel{Name: "gemini-3.1-flash-image"},
				ImageEdit: &ShortcutModel{Name: "gemini-3.1-flash-image-edit"},
			},
		},
		"medium": {
			Name: "medium", Label: "Recommended", Pricing: "$$", Color: "blue",
			Models: ShortcutModels{
				Text:      &ShortcutModel{Name: "gpt-oss-120b", Tools: cloneTools(defaultShortcutMediumTools)},
				Vision:    &ShortcutModel{Name: "gemini-3.5-flash", Tools: cloneTools(defaultShortcutMediumTools)},
				ImageGen:  &ShortcutModel{Name: "gemini-3.1-flash-image"},
				ImageEdit: &ShortcutModel{Name: "gemini-3.1-flash-image-edit"},
			},
		},
		"smart": {
			Name: "smart", Label: "Best quality but slow", Pricing: "$$$", Color: "purple",
			Models: ShortcutModels{
				Text:      &ShortcutModel{Name: "kimi-k2.6", Tools: cloneTools(defaultShortcutSmartTools)},
				Vision:    &ShortcutModel{Name: "kimi-k2.6", Tools: cloneTools(defaultShortcutSmartTools)},
				ImageGen:  &ShortcutModel{Name: "gemini-3.1-flash-image"},
				ImageEdit: &ShortcutModel{Name: "gemini-3.1-flash-image-edit"},
			},
		},
	}
}

// validateShortcuts ensures the three reserved names exist and appear first
// (in fast → medium → smart order). Custom entries trail. Returns true when
// the slice was mutated, signalling the caller to persist the config.
func validateShortcuts(c *Config) bool {
	defaults := defaultShortcuts()
	mutated := false

	// Index the existing entries by lowercased name; preserve insertion order
	// for any custom entries we encounter.
	byName := map[string]int{}
	var customOrder []int
	for i, s := range c.Shortcuts {
		key := strings.ToLower(strings.TrimSpace(s.Name))
		c.Shortcuts[i].Name = key
		byName[key] = i
		isReserved := false
		for _, r := range ReservedShortcutNames {
			if key == r {
				isReserved = true
				break
			}
		}
		if !isReserved {
			customOrder = append(customOrder, i)
		}
	}

	// Build the new ordered list: reserved first (filling in defaults), then customs.
	newList := make([]Shortcut, 0, len(c.Shortcuts)+len(ReservedShortcutNames))
	for _, name := range ReservedShortcutNames {
		if idx, ok := byName[name]; ok {
			newList = append(newList, c.Shortcuts[idx])
		} else {
			utils.Log("[Auth] shortcut %q missing, injecting default", name)
			newList = append(newList, defaults[name])
			mutated = true
		}
	}
	for _, idx := range customOrder {
		newList = append(newList, c.Shortcuts[idx])
	}

	if !mutated {
		// Still need to detect re-ordering or name normalisation as a mutation.
		if len(newList) != len(c.Shortcuts) {
			mutated = true
		} else {
			for i := range newList {
				if newList[i].Name != c.Shortcuts[i].Name {
					mutated = true
					break
				}
			}
		}
	}

	c.Shortcuts = newList
	return mutated
}
