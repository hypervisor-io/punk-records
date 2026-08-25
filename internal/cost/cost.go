// Package cost turns llm_calls into governance: per-agent daily spend,
// burn tables (avg cost per completed investigation), and burn-rate
// projection alerts. Rating happens locally at call time (llm.PriceTable);
// vendor cost APIs only reconcile daily.
package cost

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func dayStart(now time.Time) string {
	y, m, d := now.UTC().Date()
	return store.TimeToDB(time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
}

func monthStart(now time.Time) string {
	y, m, _ := now.UTC().Date()
	return store.TimeToDB(time.Date(y, m, 1, 0, 0, 0, 0, time.UTC))
}

// DailySpendByAgent returns an agent's model spend today in microUSD.
func DailySpendByAgent(ctx context.Context, db *store.DB, agent string, now time.Time) (int64, error) {
	var spend int64
	err := db.QueryRowContext(ctx, db.Rebind(`
		SELECT COALESCE(SUM(cost_microusd),0) FROM llm_calls
		WHERE agent_name = $1 AND created_at >= $2`),
		agent, dayStart(now)).Scan(&spend)
	return spend, err
}

// AgentSpend is one row of the overview.
type AgentSpend struct {
	Agent          string `json:"agent"`
	DayMicroUSD    int64  `json:"day_microusd"`
	MonthMicroUSD  int64  `json:"month_microusd"`
	CompletedTasks int64  `json:"completed_tasks"`
	AvgPerTask     int64  `json:"avg_per_task_microusd"` // the burn table
}

// Overview is GET /v1/costs.
type Overview struct {
	DayTotalMicroUSD   int64        `json:"day_total_microusd"`
	MonthTotalMicroUSD int64        `json:"month_total_microusd"`
	Agents             []AgentSpend `json:"agents"`
}

// Summarize builds the overview: day/month totals, per-agent spend, and
// average cost per completed investigation (published burn table).
func Summarize(ctx context.Context, db *store.DB, now time.Time) (*Overview, error) {
	o := &Overview{Agents: []AgentSpend{}}
	if err := db.QueryRowContext(ctx, db.Rebind(`
		SELECT COALESCE(SUM(cost_microusd),0) FROM llm_calls WHERE created_at >= $1`),
		dayStart(now)).Scan(&o.DayTotalMicroUSD); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, db.Rebind(`
		SELECT COALESCE(SUM(cost_microusd),0) FROM llm_calls WHERE created_at >= $1`),
		monthStart(now)).Scan(&o.MonthTotalMicroUSD); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, db.Rebind(`
		SELECT agent_name,
		       COALESCE(SUM(CASE WHEN created_at >= $1 THEN cost_microusd ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN created_at >= $2 THEN cost_microusd ELSE 0 END),0)
		FROM llm_calls WHERE agent_name <> ''
		GROUP BY agent_name ORDER BY agent_name`),
		dayStart(now), monthStart(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a AgentSpend
		if err := rows.Scan(&a.Agent, &a.DayMicroUSD, &a.MonthMicroUSD); err != nil {
			return nil, err
		}
		o.Agents = append(o.Agents, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range o.Agents {
		var tasks, total int64
		err := db.QueryRowContext(ctx, db.Rebind(`
			SELECT COUNT(DISTINCT t.id), COALESCE(SUM(c.cost_microusd),0)
			FROM tasks t JOIN llm_calls c ON c.task_id = t.id
			WHERE t.agent_name = $1 AND t.status = 'completed' AND t.kind = 'investigate'`),
			o.Agents[i].Agent).Scan(&tasks, &total)
		if err != nil {
			return nil, err
		}
		o.Agents[i].CompletedTasks = tasks
		if tasks > 0 {
			o.Agents[i].AvgPerTask = total / tasks
		}
	}
	return o, nil
}

// Projector emits burn-rate alerts: projected end-of-day spend crossing
// 60/80/100% of the global daily budget publishes a cost_alert bus event
// (once per level per day) - the real-time alerting every credit-meter
// vendor left out.
type Projector struct {
	DB            *store.DB
	Bus           *bus.Bus
	Log           *slog.Logger
	DailyBudgetUS int64 // microUSD; 0 disables
	Now           func() time.Time

	lastLevel int
	lastDay   string
}

func (p *Projector) Tick(ctx context.Context) error {
	if p.DailyBudgetUS <= 0 {
		return nil
	}
	now := p.Now()
	day := dayStart(now)
	if day != p.lastDay {
		p.lastDay, p.lastLevel = day, 0
	}
	var spend int64
	if err := p.DB.QueryRowContext(ctx, p.DB.Rebind(`
		SELECT COALESCE(SUM(cost_microusd),0) FROM llm_calls WHERE created_at >= $1`),
		day).Scan(&spend); err != nil {
		return err
	}
	elapsed := now.UTC().Sub(now.UTC().Truncate(24 * time.Hour))
	frac := elapsed.Hours() / 24
	if frac < 0.01 {
		frac = 0.01
	}
	projected := int64(float64(spend) / frac)
	pct := projected * 100 / p.DailyBudgetUS
	level := 0
	switch {
	case pct >= 100:
		level = 100
	case pct >= 80:
		level = 80
	case pct >= 60:
		level = 60
	}
	if level > p.lastLevel {
		p.lastLevel = level
		msg := fmt.Sprintf("projected daily spend %d%% of budget (spent %.2f USD, projected %.2f, budget %.2f)",
			pct, float64(spend)/1e6, float64(projected)/1e6, float64(p.DailyBudgetUS)/1e6)
		p.Log.Warn("cost projection alert", "level", level, "detail", msg)
		if p.Bus != nil {
			p.Bus.Publish(bus.Event{Kind: "cost_alert", Key: fmt.Sprintf("%d", level),
				Data: map[string]string{"detail": msg}})
		}
	}
	return nil
}
