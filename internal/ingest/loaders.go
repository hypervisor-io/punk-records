package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// textLoader ingests plain text verbatim as one unnamed section; the
// chunker splits it into paragraphs downstream.
type textLoader struct{}

func (textLoader) MediaType() string { return "text/plain" }

func (textLoader) Load(_ context.Context, in LoadInput) (*Result, error) {
	return &Result{Sections: []memory.DocumentSection{{Text: string(in.Body)}}}, nil
}

// markdownLoader splits a Markdown document at ATX headings (# through
// ######). Each heading starts a named section whose body keeps the
// heading line verbatim, so headings survive both as provenance (the
// section name) and as chunk text. Content before the first heading is
// an unnamed preamble section. Fenced code blocks (``` or ~~~) never
// start sections; setext headings and closing ATX hashes are kept as
// text, not interpreted.
type markdownLoader struct{}

func (markdownLoader) MediaType() string { return "text/markdown" }

func (markdownLoader) Load(_ context.Context, in LoadInput) (*Result, error) {
	var sections []memory.DocumentSection
	var cur strings.Builder
	name := ""
	open := false
	fenceMarker := "" // "" means not inside a fence; else the marker ("```" or "~~~") that opened it
	flush := func() {
		if !open {
			return
		}
		if text := strings.TrimSpace(cur.String()); text != "" {
			sections = append(sections, memory.DocumentSection{Name: name, Text: text})
		}
		cur.Reset()
		name = ""
		open = false
	}
	for _, line := range strings.Split(string(in.Body), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case fenceMarker == "" && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")):
			fenceMarker = trimmed[:3]
		case fenceMarker != "" && strings.HasPrefix(trimmed, fenceMarker):
			fenceMarker = ""
		}
		if fenceMarker == "" {
			if title, ok := atxHeading(line); ok {
				flush()
				name, open = title, true
				cur.WriteString(line)
				cur.WriteByte('\n')
				continue
			}
		}
		if !open && trimmed == "" {
			continue // no leading blank run before the first section
		}
		open = true
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	flush()
	return &Result{Sections: sections}, nil
}

// atxHeading recognizes an ATX heading line: 1-6 '#' followed by a space
// or end of line. Returns the heading text.
func atxHeading(line string) (string, bool) {
	s := strings.TrimLeft(line, " \t")
	i := 0
	for i < len(s) && s[i] == '#' {
		i++
	}
	if i == 0 || i > 6 {
		return "", false
	}
	if i < len(s) && s[i] != ' ' && s[i] != '\t' {
		return "", false
	}
	return strings.TrimSpace(s[i:]), true
}

// incidentDoc is the structured incident schema (IncidentMediaType): one
// JSON object, strictly decoded so typos surface as errors naming the
// offending field.
type incidentDoc struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Severity   string `json:"severity"`
	Summary    string `json:"summary"`
	Impact     string `json:"impact"`
	RootCause  string `json:"root_cause"`
	StartedAt  string `json:"started_at"`
	ResolvedAt string `json:"resolved_at"`
	Timeline   []struct {
		Time  string `json:"time"`
		Event string `json:"event"`
	} `json:"timeline"`
	ActionItems []struct {
		Description string `json:"description"`
		Owner       string `json:"owner"`
	} `json:"action_items"`
}

// incidentLoader turns each populated incident field into a section
// named after its JSON field ("incident" is the title/status header
// block), so every chunk's provenance names the field it came from.
type incidentLoader struct{}

func (incidentLoader) MediaType() string { return IncidentMediaType }

func (incidentLoader) Load(_ context.Context, in LoadInput) (*Result, error) {
	dec := json.NewDecoder(bytes.NewReader(in.Body))
	dec.DisallowUnknownFields()
	var doc incidentDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("incident-json: %s: %w", in.Name, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("incident-json: %s: trailing data after the incident object", in.Name)
	}
	if strings.TrimSpace(doc.Title) == "" {
		return nil, fmt.Errorf("incident-json: %s: missing required field \"title\"", in.Name)
	}
	var sections []memory.DocumentSection
	add := func(name, text string) {
		if strings.TrimSpace(text) != "" {
			sections = append(sections, memory.DocumentSection{Name: name, Text: strings.TrimSpace(text)})
		}
	}
	header := doc.Title
	if doc.ID != "" {
		header = doc.ID + ": " + doc.Title
	}
	for _, kv := range [][2]string{
		{"status", doc.Status},
		{"severity", doc.Severity},
		{"started_at", doc.StartedAt},
		{"resolved_at", doc.ResolvedAt},
	} {
		if kv[1] != "" {
			header += "\n" + kv[0] + ": " + kv[1]
		}
	}
	add("incident", header)
	add("summary", doc.Summary)
	add("impact", doc.Impact)
	add("root_cause", doc.RootCause)
	var timeline, actions []string
	for _, ev := range doc.Timeline {
		event := strings.TrimSpace(ev.Event)
		if event == "" {
			continue
		}
		if strings.TrimSpace(ev.Time) != "" {
			event = strings.TrimSpace(ev.Time) + " - " + event
		}
		timeline = append(timeline, event)
	}
	add("timeline", strings.Join(timeline, "\n\n"))
	for _, item := range doc.ActionItems {
		desc := strings.TrimSpace(item.Description)
		if desc == "" {
			continue
		}
		if strings.TrimSpace(item.Owner) != "" {
			desc += " (owner: " + strings.TrimSpace(item.Owner) + ")"
		}
		actions = append(actions, desc)
	}
	add("action_items", strings.Join(actions, "\n\n"))
	if len(sections) <= 1 {
		return nil, fmt.Errorf("incident-json: %s: %q has no content sections (want at least one of summary, impact, root_cause, timeline, action_items)", in.Name, doc.Title)
	}
	return &Result{Sections: sections}, nil
}
