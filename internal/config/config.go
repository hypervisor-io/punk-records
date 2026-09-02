// Package config loads Punk Records' configuration from a YAML file with
// environment-variable overrides. Flat, explicit keys; no magic.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/goccy/go-yaml"
)

// Config is the whole runtime configuration. Zero value is not usable;
// call Default or Load.
type Config struct {
	HTTP      HTTP      `yaml:"http"`
	Log       Log       `yaml:"log"`
	DB        DB        `yaml:"db"`
	AI        AI        `yaml:"ai"`
	Specs     Specs     `yaml:"specs"`
	Budgets   Budgets   `yaml:"budgets"`
	OTel      OTel      `yaml:"otel"`
	Memory    Memory    `yaml:"memory"`
	MCP       MCPClient `yaml:"mcp_client"`
	A2A       A2A       `yaml:"a2a"`
	Route     Route     `yaml:"route"`
	Proposals Proposals `yaml:"proposals"`
}

type HTTP struct {
	Addr string `yaml:"addr"`
}

type Log struct {
	Format string `yaml:"format"` // text | json
	Level  string `yaml:"level"`  // debug | info | warn | error
}

type DB struct {
	Driver string `yaml:"driver"` // sqlite | postgres
	DSN    string `yaml:"dsn"`
}

type AI struct {
	Enabled    bool                 `yaml:"enabled"`
	Profiles   map[string]AIProfile `yaml:"profiles"`
	Embeddings Embeddings           `yaml:"embeddings"`
}

// Embeddings configures write-time vectors (Ollama-compatible /api/embed).
// Empty model disables; FTS stays the deterministic default.
type Embeddings struct {
	Provider string `yaml:"provider"` // "" | ollama | local
	BaseURL  string `yaml:"base_url"` // ollama only
	Model    string `yaml:"model"`    // ollama: model name; local: catalog name (default potion-code-16m-v2)
	Dims     int    `yaml:"dims"`     // ollama only; local models know their width
	// MaxInputTokens is the model input window in tokens; 0 = unknown
	// (diagnose skips oversize accounting).
	MaxInputTokens int    `yaml:"max_input_tokens"`
	ModelCache     string `yaml:"model_cache"` // local only; default $PUNK_MODEL_CACHE or ~/.punk/models
}

// AIProfile mirrors llm.Profile; config stays dependency-free.
type AIProfile struct {
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	Model     string `yaml:"model"`
}

type Specs struct {
	Dir string `yaml:"dir"`
}

type Route struct {
	Epsilon  float64 `yaml:"epsilon"`  // exploration among routing ties (0 disables)
	Fallback string  `yaml:"fallback"` // agent for unmatched tasks; empty parks them
}

type Proposals struct {
	ExpireAfterHours int `yaml:"expire_after_hours"` // 0 disables the sweep
}

// Budgets are the per-task defaults; agent specs may lower (never raise)
// them per agent.
type Budgets struct {
	Tokens         int     `yaml:"tokens"`
	ToolCalls      int     `yaml:"tool_calls"`
	WallMS         int64   `yaml:"wall_ms"`
	Subagents      int     `yaml:"subagents"`
	GlobalDailyUSD float64 `yaml:"global_daily_usd"` // burn-rate projection alerts; 0 disables
	PriceTablePath string  `yaml:"price_table_path"` // override the shipped table
}

type OTel struct {
	Endpoint string `yaml:"endpoint"` // OTLP HTTP endpoint; empty disables
}

type Memory struct {
	RetentionDays      int               `yaml:"retention_days"`      // 0 disables the sweep
	ConsolidateDays    int               `yaml:"consolidate_days"`    // horizon for region compaction; 0 disables
	CreativeLinks      bool              `yaml:"creative_links"`      // add similar_to links during consolidation (needs embeddings)
	Defense            string            `yaml:"defense"`             // off | redact | block — write-time secret scrubbing
	DefensePolicies    map[string]string `yaml:"defense_policies"`    // per-namespace defense mode override
	ReconcileThreshold float64           `yaml:"reconcile_threshold"` // cosine >= this reconciles near-duplicate observations (0 disables; needs embeddings + AI)
	Entities           bool              `yaml:"entities"`            // extract + link named entities (needs AI)
	RerankerURL        string            `yaml:"reranker_url"`        // optional cross-encoder endpoint (TEI /rerank); empty disables reranking
	QuantizeVectors    bool              `yaml:"quantize_vectors"`    // true: store new embeddings int8 (4x smaller, ~2% recall cost); reads handle both
	IVFNprobe          int               `yaml:"ivf_nprobe"`          // >0 enables approximate vector index (clusters probed per query)
	IVFMinFacts        int               `yaml:"ivf_min_facts"`       // namespace size above which the index is used over brute force (0 = default 2000)
	Contradictions     bool              `yaml:"contradictions"`      // detect + link contradicting facts during consolidation (needs embeddings + AI)

	// TurnContextTokens is the token budget for per-turn context
	// injection: on every user prompt, the hook fetches a small,
	// prompt-scoped block of memory (hybrid-search hits for the prompt
	// text, minus anything already injected this session) and injects it
	// alongside the prompt. 0 disables per-turn injection entirely; the
	// session-start block is unaffected either way.
	TurnContextTokens int `yaml:"turn_context_tokens"`

	// SessionSummaryTokens enables rolling session summaries: when a
	// captured agent
	// session accumulates at least this many tokens of new events since
	// its last summary, an LLM folds prior summary + new events into an
	// updated /agent-sessions/<sid>/summary. Needs ai.enabled; 0
	// disables (the default - deterministic-first).
	SessionSummaryTokens int `yaml:"session_summary_tokens"`

	// Inject lists the components of the session-start context block.
	// Recognized: "directives" (a fixed one-line usage note telling the
	// agent to treat injected memory as background), "profile" (the
	// user's cross-project profile card from the user-profile namespace),
	// "summary" (the most recent session's rolling summary, when
	// session_summary_tokens is on), "entities" (the "Known entities"
	// line), "facts" (important/recent fact bullets). Empty/absent means
	// ["profile","summary","entities","facts"].
	Inject []string `yaml:"inject"`
}

// MCPServer is one external tool server the agents may call.
type MCPServer struct {
	Name    string   `yaml:"name"`
	Command string   `yaml:"command"` // stdio subprocess
	Args    []string `yaml:"args"`
	URL     string   `yaml:"url"`       // streamable HTTP endpoint
	Token   string   `yaml:"token_env"` // env var holding a bearer token
}

type MCPClient struct {
	Servers []MCPServer `yaml:"servers"`
}

// A2A configures outbound Agent2Agent delegation: remote agents this brain
// may hand tasks to (via the MCP `delegate` tool or `punk a2a`).
type A2A struct {
	Remotes []A2ARemote `yaml:"remotes"`
}

// A2ARemote is one foreign A2A agent, addressed by its JSON-RPC endpoint.
type A2ARemote struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`       // JSON-RPC endpoint (the AgentCard "url")
	TokenEnv string `yaml:"token_env"` // env var holding the bearer token
}

// Default returns the configuration used when no file and no env are present.
func Default() *Config {
	return &Config{
		HTTP:      HTTP{Addr: ":9090"},
		Log:       Log{Format: "text", Level: "info"},
		DB:        DB{Driver: "sqlite", DSN: "punk.db"},
		AI:        AI{Enabled: false},
		Specs:     Specs{Dir: "./specs"},
		Budgets:   Budgets{Tokens: 200_000, ToolCalls: 50, WallMS: 600_000, Subagents: 3},
		OTel:      OTel{Endpoint: ""},
		Memory:    Memory{RetentionDays: 0, TurnContextTokens: 600},
		Route:     Route{Epsilon: 0.05},
		Proposals: Proposals{ExpireAfterHours: 72},
	}
}

// Load reads path (optional: missing file is fine, defaults apply), then
// applies PUNK_* environment overrides, then validates.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// defaults + env only
		case err != nil:
			return nil, fmt.Errorf("read config: %w", err)
		default:
			if err := yaml.Unmarshal(raw, c); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}
	if err := applyEnv(c); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) error {
	var errs []error
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*dst = v
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
				return
			}
			*dst = b
		}
	}
	integer := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
				return
			}
			*dst = n
		}
	}
	int64v := func(key string, dst *int64) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
				return
			}
			*dst = n
		}
	}

	str("PUNK_HTTP_ADDR", &c.HTTP.Addr)
	str("PUNK_LOG_FORMAT", &c.Log.Format)
	str("PUNK_LOG_LEVEL", &c.Log.Level)
	str("PUNK_DB_DRIVER", &c.DB.Driver)
	str("PUNK_DB_DSN", &c.DB.DSN)
	boolean("PUNK_AI_ENABLED", &c.AI.Enabled)
	str("PUNK_SPECS_DIR", &c.Specs.Dir)
	integer("PUNK_BUDGET_TOKENS", &c.Budgets.Tokens)
	integer("PUNK_BUDGET_TOOL_CALLS", &c.Budgets.ToolCalls)
	int64v("PUNK_BUDGET_WALL_MS", &c.Budgets.WallMS)
	integer("PUNK_BUDGET_SUBAGENTS", &c.Budgets.Subagents)
	str("PUNK_OTEL_ENDPOINT", &c.OTel.Endpoint)
	integer("PUNK_MEMORY_RETENTION_DAYS", &c.Memory.RetentionDays)
	integer("PUNK_MEMORY_TURN_CONTEXT_TOKENS", &c.Memory.TurnContextTokens)

	return errors.Join(errs...)
}

func (c *Config) validate() error {
	var errs []error
	switch c.Log.Format {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format %q: want text or json", c.Log.Format))
	}
	switch c.DB.Driver {
	case "sqlite", "postgres":
	default:
		errs = append(errs, fmt.Errorf("db.driver %q: want sqlite or postgres", c.DB.Driver))
	}
	if c.AI.Enabled {
		if len(c.AI.Profiles) == 0 {
			errs = append(errs, errors.New("ai.enabled=true requires at least one ai.profiles entry"))
		}
		for name, p := range c.AI.Profiles {
			if p.Model == "" {
				errs = append(errs, fmt.Errorf("ai.profiles.%s: model is required", name))
			}
		}
	}
	if c.HTTP.Addr == "" {
		errs = append(errs, errors.New("http.addr: empty"))
	}
	if c.Budgets.Tokens <= 0 || c.Budgets.ToolCalls <= 0 || c.Budgets.WallMS <= 0 || c.Budgets.Subagents < 0 {
		errs = append(errs, errors.New("budgets: tokens, tool_calls, wall_ms must be > 0 and subagents >= 0"))
	}
	if c.Memory.RetentionDays < 0 {
		errs = append(errs, errors.New("memory.retention_days: must be >= 0"))
	}
	if c.AI.Embeddings.MaxInputTokens < 0 {
		errs = append(errs, errors.New("ai.embeddings.max_input_tokens: must be >= 0"))
	}
	if p := c.AI.Embeddings.Provider; p != "" && p != "ollama" && p != "local" {
		errs = append(errs, fmt.Errorf("ai.embeddings.provider: %q, want ollama or local", p))
	}
	if c.Memory.IVFNprobe < 0 || c.Memory.IVFMinFacts < 0 {
		errs = append(errs, errors.New("memory.ivf_nprobe, memory.ivf_min_facts: must be >= 0"))
	}
	if c.Memory.TurnContextTokens < 0 {
		errs = append(errs, errors.New("memory.turn_context_tokens: must be >= 0"))
	}
	if c.Memory.SessionSummaryTokens < 0 {
		errs = append(errs, errors.New("memory.session_summary_tokens: must be >= 0"))
	}
	for _, comp := range c.Memory.Inject {
		switch comp {
		case "directives", "profile", "summary", "entities", "facts":
		default:
			errs = append(errs, fmt.Errorf("memory.inject %q: want directives, profile, summary, entities, or facts", comp))
		}
	}
	switch c.Memory.Defense {
	case "", "off", "redact", "block":
	default:
		errs = append(errs, fmt.Errorf("memory.defense %q: want off, redact, or block", c.Memory.Defense))
	}
	for ns, mode := range c.Memory.DefensePolicies {
		switch mode {
		case "", "off", "redact", "block":
		default:
			errs = append(errs, fmt.Errorf("memory.defense_policies[%s] %q: want off, redact, or block", ns, mode))
		}
	}
	if c.Route.Epsilon < 0 || c.Route.Epsilon >= 1 {
		errs = append(errs, errors.New("route.epsilon: must be in [0,1)"))
	}
	if c.Proposals.ExpireAfterHours < 0 {
		errs = append(errs, errors.New("proposals.expire_after_hours: must be >= 0"))
	}
	return errors.Join(errs...)
}
