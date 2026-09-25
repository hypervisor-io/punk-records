package a2a

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/task"
)

func TestStateFromLedger(t *testing.T) {
	cases := map[string]TaskState{
		task.StatusSubmitted:     StateSubmitted,
		task.StatusWorking:       StateWorking,
		task.StatusInputRequired: StateInputRequired, // underscore -> kebab
		task.StatusCompleted:     StateCompleted,
		task.StatusFailed:        StateFailed,
		task.StatusCanceled:      StateCanceled,
		"bogus":                  StateUnknown,
		"":                       StateUnknown,
	}
	for in, want := range cases {
		if got := StateFromLedger(in); got != want {
			t.Errorf("StateFromLedger(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTaskStateTerminal(t *testing.T) {
	terminal := []TaskState{StateCompleted, StateCanceled, StateFailed, StateRejected}
	open := []TaskState{StateSubmitted, StateWorking, StateInputRequired, StateAuthRequired, StateUnknown}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range open {
		if s.Terminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
}

func msgEvent(t *testing.T, seq int64, id, text string) task.Event {
	t.Helper()
	b, err := EncodeMessage(Message{MessageID: id, Role: RoleUser, Parts: []Part{{Kind: KindTextPart, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	return task.Event{Seq: seq, Type: task.EventMessage, Actor: "user", Payload: b}
}

func TestTaskFromLedgerMapsStatusHistoryAndArtifacts(t *testing.T) {
	updated := time.Date(2026, 9, 8, 12, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))
	lt := &task.Task{
		ID:              "t-1",
		Source:          "mcp",
		Kind:            "investigate",
		Status:          task.StatusWorking,
		AgentName:       "sre",
		HandoffContract: "findings",
		Labels:          map[string]string{ContextLabel: "ctx-9"},
		UpdatedAt:       updated,
	}
	events := []task.Event{
		msgEvent(t, 1, "m1", "hello"),
		{Seq: 2, Type: task.EventStatusChange, Payload: json.RawMessage(`{"status":"working"}`)},
		{Seq: 3, Type: task.EventFinding, Actor: "sre", Payload: json.RawMessage(`{"finding":{"title":"Disk full","summary":"root at 99%"}}`)},
		msgEvent(t, 4, "m2", "any update?"),
	}

	got := TaskFromLedger(lt, events, 0)

	if got.Kind != KindTask || got.ID != "t-1" || got.ContextID != "ctx-9" {
		t.Fatalf("identity: %+v", got)
	}
	if got.Status.State != StateWorking {
		t.Fatalf("state = %q", got.Status.State)
	}
	if got.Status.Timestamp != "2026-09-08T06:30:00Z" {
		t.Fatalf("timestamp should be UTC RFC3339, got %q", got.Status.Timestamp)
	}
	if got.Metadata["source"] != "mcp" || got.Metadata["kind"] != "investigate" ||
		got.Metadata["handoffContract"] != "findings" || got.Metadata["agent"] != "sre" {
		t.Fatalf("metadata: %+v", got.Metadata)
	}
	if len(got.History) != 2 || got.History[0].MessageID != "m1" || got.History[1].MessageID != "m2" {
		t.Fatalf("history: %+v", got.History)
	}
	if got.History[0].Kind != KindMessage {
		t.Fatalf("history kind should be normalized to %q, got %q", KindMessage, got.History[0].Kind)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("artifacts: %+v", got.Artifacts)
	}
	a := got.Artifacts[0]
	if a.ArtifactID != "finding-3" || a.Name != "Disk full" {
		t.Fatalf("artifact id/name: %+v", a)
	}
	if len(a.Parts) != 1 || a.Parts[0].Kind != KindDataPart {
		t.Fatalf("artifact parts: %+v", a.Parts)
	}
	var body map[string]string
	if err := json.Unmarshal(a.Parts[0].Data, &body); err != nil || body["summary"] != "root at 99%" {
		t.Fatalf("artifact data should be the unwrapped finding, got %s (%v)", a.Parts[0].Data, err)
	}
	if a.Metadata["seq"] != int64(3) || a.Metadata["actor"] != "sre" {
		t.Fatalf("artifact metadata: %+v", a.Metadata)
	}
}

func TestTaskFromLedgerOmitsAgentWhenUnassigned(t *testing.T) {
	got := TaskFromLedger(&task.Task{ID: "t", Status: task.StatusSubmitted}, nil, 0)
	if _, ok := got.Metadata["agent"]; ok {
		t.Fatal("agent metadata should be absent when AgentName is empty")
	}
	if got.Status.State != StateSubmitted || got.History != nil || got.Artifacts != nil {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestTaskFromLedgerHistoryLengthKeepsNewest(t *testing.T) {
	lt := &task.Task{ID: "t", Status: task.StatusWorking}
	events := []task.Event{
		msgEvent(t, 1, "m1", "a"),
		msgEvent(t, 2, "m2", "b"),
		msgEvent(t, 3, "m3", "c"),
	}
	got := TaskFromLedger(lt, events, 2)
	if len(got.History) != 2 || got.History[0].MessageID != "m2" || got.History[1].MessageID != "m3" {
		t.Fatalf("history should keep the two newest, got %+v", got.History)
	}
	if all := TaskFromLedger(lt, events, 0); len(all.History) != 3 {
		t.Fatalf("historyLength 0 should mean all, got %d", len(all.History))
	}
	if more := TaskFromLedger(lt, events, 10); len(more.History) != 3 {
		t.Fatalf("historyLength above count should not truncate, got %d", len(more.History))
	}
}

func TestTaskFromLedgerSkipsMalformedMessages(t *testing.T) {
	lt := &task.Task{ID: "t", Status: task.StatusWorking}
	events := []task.Event{
		{Seq: 1, Type: task.EventMessage, Payload: json.RawMessage(`not json`)},
		{Seq: 2, Type: task.EventMessage, Payload: json.RawMessage(`{"role":"user"}`)}, // no messageId
		msgEvent(t, 3, "ok", "fine"),
	}
	got := TaskFromLedger(lt, events, 0)
	if len(got.History) != 1 || got.History[0].MessageID != "ok" {
		t.Fatalf("history: %+v", got.History)
	}
}

func TestArtifactFromFinding(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantOK   bool
		wantName string
	}{
		{"wrapped with title", `{"finding":{"title":"T","summary":"S"}}`, true, "T"},
		{"wrapped summary only", `{"finding":{"summary":"S"}}`, true, "S"},
		{"unwrapped finding", `{"title":"Bare"}`, true, "Bare"},
		{"no name fields", `{"finding":{"severity":"low"}}`, true, "finding"},
		{"empty payload", ``, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := artifactFromFinding(task.Event{Seq: 7, Type: task.EventFinding, Payload: json.RawMessage(tc.payload)})
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if a.Name != tc.wantName || a.ArtifactID != "finding-7" {
				t.Fatalf("got name %q id %q", a.Name, a.ArtifactID)
			}
		})
	}
}

func TestItoa(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 7: "7", 42: "42", -3: "-3", 9223372036854775807: "9223372036854775807"} {
		if got := itoa(n); got != want {
			t.Errorf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestMessageText(t *testing.T) {
	m := Message{Parts: []Part{
		{Kind: KindTextPart, Text: "one"},
		{Kind: KindDataPart, Data: json.RawMessage(`{"x":1}`)},
		{Kind: KindTextPart, Text: "two"},
	}}
	if got := m.Text(); got != "one\ntwo" {
		t.Fatalf("Text() = %q", got)
	}
	if got := (Message{}).Text(); got != "" {
		t.Fatalf("empty Text() = %q", got)
	}
}

func TestEncodeMessageNormalizesKind(t *testing.T) {
	b, err := EncodeMessage(Message{MessageID: "m", Kind: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	var back Message
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Kind != KindMessage || back.MessageID != "m" {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestTextMessage(t *testing.T) {
	m := TextMessage("hi")
	if m.Kind != KindMessage || m.Role != RoleUser || m.Text() != "hi" {
		t.Fatalf("%+v", m)
	}
}
