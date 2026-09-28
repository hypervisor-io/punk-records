// Package nativewake wakes native host sessions when their durable Punk inbox
// changes. Wake notifications never lease or acknowledge peer messages.
package nativewake

import (
	"context"
	"errors"
	"time"
)

var (
	ErrBusy        = errors.New("native session busy or status unknown")
	ErrUnavailable = errors.New("native session unavailable")
	// ErrStateLocked marks a per-identity state dir held by another live
	// wake worker. Run wraps it (after its bounded acquisition wait) so
	// the lifecycle layer can tell "previous generation still draining"
	// apart from definite failures and retry honestly.
	ErrStateLocked = errors.New("state dir already locked")
)

// Outcome records host handoff only, never model consumption or task completion.
type Outcome struct {
	Accepted    bool
	Unconfirmed bool
}

// Transport connects to an existing session; it must never create a new one.
type Transport interface {
	Probe(context.Context) error
	Wake(context.Context, string) (Outcome, error)
	Close() error
}

// Config is local worker configuration. APIKey is private and must not appear in
// persistent state, command-line arguments, diagnostics or log output.
type Config struct {
	Client         string        `json:"client"`
	SessionID      string        `json:"session_id"`
	Namespace      string        `json:"namespace"`
	URL            string        `json:"url"`
	APIKey         string        `json:"api_key,omitempty"`
	StateDir       string        `json:"state_dir"`
	ControlPath    string        `json:"control_path,omitempty"`
	Generation     string        `json:"generation,omitempty"`
	MaxWakes       int           `json:"max_wakes"`
	Window         time.Duration `json:"window"`
	Cooldown       time.Duration `json:"cooldown"`
	SenderPrefixes []string      `json:"sender_prefixes,omitempty"`
}
