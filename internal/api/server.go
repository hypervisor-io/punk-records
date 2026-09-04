// Package api is the HTTP surface: health probes now, /v1 REST later (P2+).
package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/policy"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/route"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// Deps carries the subsystems the API exposes. Nil members leave their
// routes unregistered (nothing merges unwired; tests wire what they test).
type Deps struct {
	Memory        *memory.Store
	Ledger        *task.Ledger
	Router        *route.Router
	Proposals     *policy.Proposals
	Keys          *Keys // nil disables auth entirely (unit tests)
	Bus           *bus.Bus
	DB            *store.DB // cost summaries
	Reg           *registry.Registry
	Region        *region.Store        // nil: brain snapshot reports no members or claims
	Expander      memory.QueryExpander // nil disables search's expand param (deterministic-first)
	DefaultBudget task.Budget

	// TurnContextTokens is the token budget for the per-turn (mode=turn)
	// path of GET /v1/agent/context. 0 disables that mode:
	// it answers an empty context so hook clients stay no-ops.
	TurnContextTokens int
	// Inject lists the session-start context components ("directives",
	// "profile", "entities", "facts"). Empty means the default:
	// profile, entities, facts.
	Inject []string
}

// Server owns the HTTP router.
type Server struct {
	log           *slog.Logger
	mux           *chi.Mux
	mem           *memory.Store
	ledger        *task.Ledger
	router        *route.Router
	props         *policy.Proposals
	keys          *Keys
	bus           *bus.Bus
	db            *store.DB
	reg           *registry.Registry
	region        *region.Store
	expander      memory.QueryExpander
	defaultBudget task.Budget
	turnTokens    int      // per-turn context budget; 0 disables mode=turn
	inject        []string // session-start context components; empty = default
	version       string   // stamped via MountAgentCard; used in A2A cards

	// Ready reports readiness (DB reachable, registry loaded). Nil means
	// "ready" so the skeleton stays honest before P1 wires the store.
	Ready func() error
}

func New(log *slog.Logger, d Deps) *Server {
	s := &Server{
		log: log, mux: chi.NewRouter(),
		mem: d.Memory, ledger: d.Ledger, router: d.Router, props: d.Proposals,
		keys: d.Keys, bus: d.Bus, db: d.DB, reg: d.Reg, region: d.Region, expander: d.Expander,
		defaultBudget: d.DefaultBudget,
		turnTokens:    d.TurnContextTokens, inject: d.Inject,
	}
	s.mux.Use(middleware.RequestID)
	s.mux.Use(middleware.Recoverer)
	s.mux.Get("/healthz", s.handleHealthz)
	s.mux.Get("/readyz", s.handleReadyz)
	s.mux.Route("/v1", func(v1 chi.Router) {
		v1.Use(s.authMiddleware)
		if s.mem != nil {
			v1.Route("/namespaces/{ns}", func(r chi.Router) {
				r.Post("/memories", s.handleRemember)
				r.Get("/memories", s.handleRecall)
				r.Delete("/memories", s.handleForget)
				r.Get("/memories/search", s.handleSearch)
				r.Get("/keys", s.handleListKeys)
				r.Get("/events", s.handleMemoryEvents)
				r.Get("/tasks", s.handleTaskBoard)
				r.Post("/tasks/{id}/status", s.handleTaskStatus)
				r.Get("/profile", s.handleProfile)
				r.Get("/diagnose", s.handleDiagnose)
			})
			v1.Group(func(ag chi.Router) {
				ag.Use(s.rateLimit(50, 100)) // hooks fire on every tool call
				ag.Post("/agent/hooks", s.handleAgentHook)
				ag.Get("/agent/context", s.handleAgentContext)
				ag.Get("/agent/namespace", s.handleAgentNamespace)
			})
		}
		if s.mem != nil {
			v1.Get("/brain/snapshot", s.handleBrainSnapshot)
			v1.Get("/brain/events", s.handleBrainEvents)
		}
		if s.ledger != nil && s.router != nil {
			v1.Route("/tasks", func(r chi.Router) {
				r.Post("/", s.handleSubmitTask)
				r.Get("/", s.handleListTasks)
				r.Get("/{id}", s.handleGetTask)
				r.Post("/{id}/cancel", s.handleCancelTask)
				r.Post("/{id}/requeue", s.handleRequeueTask)
				if s.props != nil {
					r.Get("/{id}/proposals", s.handleListTaskProposals)
				}
			})
			// A2A (Agent2Agent) JSON-RPC transport over the same ledger.
			v1.Post("/a2a", s.handleA2A)
			v1.Group(func(in chi.Router) {
				in.Use(s.rateLimit(20, 40)) // per-IP; webhooks retry, they must not grind us
				in.Post("/intake/webhook", s.handleIntakeWebhook)
			})
		}
		if s.props != nil {
			v1.Patch("/proposals/{id}", s.handlePatchProposal)
			v1.Get("/proposals", s.handleListProposals)
		}
		if s.reg != nil {
			v1.Get("/agents", s.handleListAgents)
			v1.Get("/agents/cards", s.handleAgentCardList)   // A2A discovery
			v1.Get("/agents/{name}/card", s.handleAgentCard) // A2A Agent Card
		}
		if s.db != nil {
			v1.Get("/costs", s.handleCosts)
		}
	})
	return s
}

// Router exposes the http.Handler for the std http.Server in cmd.
func (s *Server) Router() http.Handler { return s.mux }

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Ready != nil {
		if err := s.Ready(); err != nil {
			s.log.Warn("readyz failing", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not ready"}`))
			return
		}
	}
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

// MountMCP exposes the MCP streamable-HTTP endpoint at /mcp behind the
// same bearer auth as /v1.
func (s *Server) MountMCP(h http.Handler) {
	s.mux.With(s.authMiddleware).Handle("/mcp", h)
}
