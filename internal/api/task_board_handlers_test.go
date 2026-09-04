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
