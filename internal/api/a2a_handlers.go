package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hypervisor-io/punk-records/internal/a2a"
)

// buildAgentCard assembles the A2A v0.3 Agent Card describing this Punk
// Records deployment as an A2A agent: identity, the JSON-RPC endpoint, its
// capabilities, and one skill per registered (enabled) agent. Served both
// at /.well-known/agent-card.json and via agent/getAuthenticatedExtendedCard.
func (s *Server) buildAgentCard() map[string]any {
	version := s.version
	if version == "" {
		version = "dev"
	}
	type skill struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	}
	skills := []skill{}
	if snap := s.reg.Current(); snap != nil {
		for _, a := range snap.Bundle.Agents {
			if a.Disabled {
				continue
			}
			skills = append(skills, skill{
				ID: a.Name, Name: a.Name, Description: a.Description,
				Tags: []string{"autonomy:" + a.Autonomy, "version:" + a.Version},
			})
		}
	}
	return map[string]any{
		"protocolVersion":    a2a.ProtocolVersion,
		"name":               "punk-records",
		"description":        "Shared brain for AI agents: append-only regions, conflict-free coordination, task delegation over A2A + MCP.",
		"version":            version,
		"url":                "/v1/a2a",
		"preferredTransport": "JSONRPC",
		"capabilities": map[string]any{
			"streaming":              true,
			"pushNotifications":      s.db != nil,
			"stateTransitionHistory": true,
		},
		"defaultInputModes":  []string{"text/plain", "application/json"},
		"defaultOutputModes": []string{"text/plain", "application/json"},
		"securitySchemes": map[string]any{
			"bearer": map[string]any{"type": "http", "scheme": "bearer"},
		},
		"security": []map[string][]string{{"bearer": {}}},
		"skills":   skills,
	}
}

// A2A Agent Card shape (subset): name, description, skills, url. Serving
// cards is the cheap, useful half of A2A - discovery - without adopting
// the full task transport (our ledger already is A2A-shaped). Full A2A
// endpoints are backlog; this makes agents discoverable to A2A clients
// and hyperscaler platforms now (P18.2 partial).
type agentSkillCard struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
}

type agentCard struct {
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	Version      string           `json:"version"`
	URL          string           `json:"url"`
	Capabilities map[string]bool  `json:"capabilities"`
	Skills       []agentSkillCard `json:"skills"`
}

func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	snap := s.reg.Current()
	name := chi.URLParam(r, "name")
	if snap == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no snapshot"})
		return
	}
	a, ok := snap.Bundle.Agents[name]
	if !ok || a.Disabled {
		writeErr(w, http.StatusNotFound, errNotFound)
		return
	}
	card := agentCard{
		Name: a.Name, Description: a.Description, Version: a.Version,
		URL:          "/v1/tasks",
		Capabilities: map[string]bool{"streaming": false, "pushNotifications": true},
		Skills:       []agentSkillCard{},
	}
	for _, skName := range a.Skills {
		sk := agentSkillCard{ID: skName, Name: skName}
		if s, ok := snap.Bundle.Skills[skName]; ok {
			sk.Description = s.Description
		}
		card.Skills = append(card.Skills, sk)
	}
	writeJSON(w, http.StatusOK, card)
}

func (s *Server) handleAgentCardList(w http.ResponseWriter, r *http.Request) {
	snap := s.reg.Current()
	cards := []agentCard{}
	if snap != nil {
		for name := range snap.Bundle.Agents {
			a := snap.Bundle.Agents[name]
			if a.Disabled {
				continue
			}
			cards = append(cards, agentCard{Name: a.Name, Description: a.Description,
				Version: a.Version, URL: "/v1/tasks",
				Capabilities: map[string]bool{"pushNotifications": true}})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": cards})
}
