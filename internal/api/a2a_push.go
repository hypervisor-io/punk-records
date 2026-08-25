package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/hypervisor-io/punk-records/internal/a2a"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// a2aPushClient delivers push notifications; short timeout, best-effort.
var a2aPushClient = &http.Client{Timeout: 10 * time.Second}

// ---- store (a2a_push_configs table) ----

func (s *Server) pushSet(ctx context.Context, taskID string, cfg a2a.PushNotificationConfig) (a2a.PushNotificationConfig, error) {
	if cfg.ID == "" {
		cfg.ID = randID("push")
	}
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`
		INSERT INTO a2a_push_configs (task_id, config_id, url, token, created_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (task_id, config_id) DO UPDATE SET url = $3, token = $4`),
		taskID, cfg.ID, cfg.URL, cfg.Token, store.TimeToDB(time.Now()))
	return cfg, err
}

func (s *Server) pushList(ctx context.Context, taskID string) ([]a2a.PushNotificationConfig, error) {
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(
		`SELECT config_id, url, token FROM a2a_push_configs WHERE task_id = $1 ORDER BY id`), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []a2a.PushNotificationConfig
	for rows.Next() {
		var c a2a.PushNotificationConfig
		if err := rows.Scan(&c.ID, &c.URL, &c.Token); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Server) pushDelete(ctx context.Context, taskID, configID string) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(
		`DELETE FROM a2a_push_configs WHERE task_id = $1 AND config_id = $2`), taskID, configID)
	return err
}

// ---- RPC handlers ----

func (s *Server) a2aPushSet(w http.ResponseWriter, r *http.Request, req a2a.Request) {
	if s.db == nil {
		writeRPCError(w, req.ID, a2a.ErrPushNotSupported, "push notifications not supported", nil)
		return
	}
	var p a2a.TaskPushNotificationConfig
	if err := json.Unmarshal(req.Params, &p); err != nil || p.TaskID == "" || p.PushNotificationConfig.URL == "" {
		writeRPCError(w, req.ID, a2a.ErrInvalidParams, "invalid params: taskId and pushNotificationConfig.url required", nil)
		return
	}
	if _, _, err := s.ledger.Get(r.Context(), p.TaskID); err != nil {
		writeRPCError(w, req.ID, a2a.ErrTaskNotFound, "task not found: "+p.TaskID, nil)
		return
	}
	cfg, err := s.pushSet(r.Context(), p.TaskID, p.PushNotificationConfig)
	if err != nil {
		writeRPCError(w, req.ID, a2a.ErrInternal, err.Error(), nil)
		return
	}
	writeRPCResult(w, req.ID, a2a.TaskPushNotificationConfig{TaskID: p.TaskID, PushNotificationConfig: cfg})
}

func (s *Server) a2aPushGet(w http.ResponseWriter, r *http.Request, req a2a.Request) {
	if s.db == nil {
		writeRPCError(w, req.ID, a2a.ErrPushNotSupported, "push notifications not supported", nil)
		return
	}
	var p struct {
		ID                     string `json:"id"`
		PushNotificationConfID string `json:"pushNotificationConfigId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" {
		writeRPCError(w, req.ID, a2a.ErrInvalidParams, "invalid params: id required", nil)
		return
	}
	cfgs, err := s.pushList(r.Context(), p.ID)
	if err != nil {
		writeRPCError(w, req.ID, a2a.ErrInternal, err.Error(), nil)
		return
	}
	for _, c := range cfgs {
		if p.PushNotificationConfID == "" || c.ID == p.PushNotificationConfID {
			writeRPCResult(w, req.ID, a2a.TaskPushNotificationConfig{TaskID: p.ID, PushNotificationConfig: c})
			return
		}
	}
	writeRPCError(w, req.ID, a2a.ErrTaskNotFound, "no push config for task", nil)
}

func (s *Server) a2aPushList(w http.ResponseWriter, r *http.Request, req a2a.Request) {
	if s.db == nil {
		writeRPCError(w, req.ID, a2a.ErrPushNotSupported, "push notifications not supported", nil)
		return
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" {
		writeRPCError(w, req.ID, a2a.ErrInvalidParams, "invalid params: id required", nil)
		return
	}
	cfgs, err := s.pushList(r.Context(), p.ID)
	if err != nil {
		writeRPCError(w, req.ID, a2a.ErrInternal, err.Error(), nil)
		return
	}
	out := make([]a2a.TaskPushNotificationConfig, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, a2a.TaskPushNotificationConfig{TaskID: p.ID, PushNotificationConfig: c})
	}
	writeRPCResult(w, req.ID, out)
}

func (s *Server) a2aPushDelete(w http.ResponseWriter, r *http.Request, req a2a.Request) {
	if s.db == nil {
		writeRPCError(w, req.ID, a2a.ErrPushNotSupported, "push notifications not supported", nil)
		return
	}
	var p struct {
		ID                     string `json:"id"`
		PushNotificationConfID string `json:"pushNotificationConfigId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" || p.PushNotificationConfID == "" {
		writeRPCError(w, req.ID, a2a.ErrInvalidParams, "invalid params: id and pushNotificationConfigId required", nil)
		return
	}
	if err := s.pushDelete(r.Context(), p.ID, p.PushNotificationConfID); err != nil {
		writeRPCError(w, req.ID, a2a.ErrInternal, err.Error(), nil)
		return
	}
	writeRPCResult(w, req.ID, map[string]any{"deleted": true})
}

// ---- delivery ----

// StartA2APushDelivery subscribes to the bus and POSTs the A2A Task to every
// registered webhook when a task reaches a terminal state. Best-effort: a
// failed webhook is logged, not retried (durable delivery is backlog).
func (s *Server) StartA2APushDelivery(ctx context.Context) {
	if s.db == nil || s.bus == nil {
		return
	}
	events, cancel := s.bus.Subscribe()
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case e, open := <-events:
				if !open {
					return
				}
				if e.Kind != "task_status" || !a2a.StateFromLedger(e.Data["status"]).Terminal() {
					continue
				}
				s.deliverPush(ctx, e.Key)
			}
		}
	}()
}

func (s *Server) deliverPush(ctx context.Context, taskID string) {
	cfgs, err := s.pushList(ctx, taskID)
	if err != nil || len(cfgs) == 0 {
		return
	}
	tk, evs, err := s.ledger.Get(ctx, taskID)
	if err != nil {
		return
	}
	body, err := json.Marshal(a2a.TaskFromLedger(tk, evs, 0))
	if err != nil {
		return
	}
	for _, c := range cfgs {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if c.Token != "" {
			req.Header.Set("X-A2A-Notification-Token", c.Token)
		}
		resp, err := a2aPushClient.Do(req)
		if err != nil {
			s.log.Warn("a2a push delivery failed", "task", taskID, "url", c.URL, "err", err)
			continue
		}
		_ = resp.Body.Close()
	}
}
