package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
)

func TestTaskBoardRoutes(t *testing.T) {
	s, _, _ := brainTestServer(t)
	s.bus = bus.New() // brainTestServer wires no bus; without one ?wait= returns at once
	ns := "/v1/namespaces/board"
	rr := do(t, s, http.MethodPost, ns+"/memories", `{"key":"/tasks/A","body":"first","author":"planner"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", rr.Code, rr.Body)
	}
	rr = do(t, s, http.MethodPost, ns+"/memories", `{"key":"/tasks/B","body":"second\ndepends_on: A","author":"planner"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", rr.Code, rr.Body)
	}

	rr = do(t, s, http.MethodGet, ns+"/tasks", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"next":"A"`) {
		t.Fatalf("board = %d: %s", rr.Code, rr.Body)
	}

	rr = do(t, s, http.MethodPost, ns+"/tasks/A/status", `{"state":"done","summary":"landed","sha":"abc","tests":"go test ./...","agent":"w1"}`)
	if rr.Code != http.StatusCreated || !strings.Contains(rr.Body.String(), `"body":"done: abc landed; tests: go test ./..."`) {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	rr = do(t, s, http.MethodPost, ns+"/tasks/A/status", `{"state":"finished","summary":"x"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad state = %d", rr.Code)
	}

	rr = do(t, s, http.MethodGet, ns+"/tasks?state=pending", "")
	var board struct {
		Next  string                `json:"next"`
		Tasks []struct{ ID string } `json:"tasks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &board); err != nil || board.Next != "B" || len(board.Tasks) != 1 {
		t.Fatalf("filtered = %v %s", err, rr.Body)
	}

	start := time.Now()
	rr = do(t, s, http.MethodGet, ns+"/tasks?wait=1", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"changed":false`) || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("wait = %d after %s: %s", rr.Code, time.Since(start), rr.Body)
	}
}

// TestTaskBoardPaging: ?limit/?offset page the rows after the state
// filter, total reports the filtered count, counts stay whole-board, and
// malformed paging is a 400.
func TestTaskBoardPaging(t *testing.T) {
	s, _, _ := brainTestServer(t)
	ns := "/v1/namespaces/paged"
	for _, id := range []string{"A", "B", "C", "D", "E"} {
		if rr := do(t, s, http.MethodPost, ns+"/memories", `{"key":"/tasks/`+id+`","body":"task `+id+`","author":"planner"}`); rr.Code != http.StatusCreated {
			t.Fatalf("remember %s = %d: %s", id, rr.Code, rr.Body)
		}
	}
	if rr := do(t, s, http.MethodPost, ns+"/tasks/E/status", `{"state":"done","summary":"x"}`); rr.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	type page struct {
		Total  int                   `json:"total"`
		Limit  int                   `json:"limit"`
		Offset int                   `json:"offset"`
		Counts struct{ Total int }   `json:"counts"`
		Tasks  []struct{ ID string } `json:"tasks"`
	}
	get := func(q string) page {
		t.Helper()
		rr := do(t, s, http.MethodGet, ns+"/tasks"+q, "")
		var p page
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &p) != nil {
			t.Fatalf("board%s = %d: %s", q, rr.Code, rr.Body)
		}
		return p
	}
	ids := func(p page) string {
		var out []string
		for _, x := range p.Tasks {
			out = append(out, x.ID)
		}
		return strings.Join(out, ",")
	}
	if p := get(""); p.Total != 5 || ids(p) != "A,B,C,D,E" {
		t.Fatalf("unpaged = %+v", p)
	}
	if p := get("?limit=2&offset=1"); p.Total != 5 || p.Counts.Total != 5 || p.Limit != 2 || p.Offset != 1 || ids(p) != "B,C" {
		t.Fatalf("page = %+v", p)
	}
	if p := get("?state=pending&limit=3&offset=2"); p.Total != 4 || ids(p) != "C,D" {
		t.Fatalf("filtered page = %+v", p)
	}
	if p := get("?state=open"); p.Total != 4 || ids(p) != "A,B,C,D" {
		t.Fatalf("open = %+v", p)
	}
	if rr := do(t, s, http.MethodPost, ns+"/tasks/A/status", `{"state":"open","summary":"x"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("open as a status = %d, want 400", rr.Code)
	}
	if p := get("?limit=2&offset=9"); p.Total != 5 || len(p.Tasks) != 0 {
		t.Fatalf("past the end = %+v", p)
	}
	for _, q := range []string{"?limit=-1", "?offset=x", "?limit=1.5"} {
		if rr := do(t, s, http.MethodGet, ns+"/tasks"+q, ""); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", q, rr.Code)
		}
	}
}

// TestRecentOffset: ?offset pages the recent feed in the same order.
func TestRecentOffset(t *testing.T) {
	c := newClockedServer(t)
	for i, k := range []string{"/a", "/b", "/c"} {
		c.write(t, time.Duration(i)*time.Second, "feed", k, "v")
	}
	keys := func(q string) string {
		t.Helper()
		rr := do(t, c.Server, http.MethodGet, "/v1/namespaces/feed/recent"+q, "")
		var out struct {
			Facts []struct{ Key string } `json:"facts"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &out) != nil {
			t.Fatalf("recent%s = %d: %s", q, rr.Code, rr.Body)
		}
		var ks []string
		for _, f := range out.Facts {
			ks = append(ks, f.Key)
		}
		return strings.Join(ks, ",")
	}
	if got := keys("?limit=2"); got != "/c,/b" {
		t.Fatalf("first page = %s", got)
	}
	if got := keys("?limit=2&offset=2"); got != "/a" {
		t.Fatalf("second page = %s", got)
	}
}
