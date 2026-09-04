package hookcli

import (
	"strings"
	"text/template"
)

// PlanSkillName is the planner-side companion of punk-memory: how one
// session splits work into /tasks facts, hands them to other agents,
// and gates the result. punk connect installs both skills side by side.
const PlanSkillName = "punk-plan"

const planDescription = "Plan and gate multi-agent work through punk-records: create a coordination namespace, write /plan/summary, conventions and one /tasks fact per task with depends_on, leave a pointer in the repo namespace, hand workers a prompt, then gate with list_tasks and await_tasks, review each finished task, and release. Use when splitting a feature into tasks for other agents, or when asked to coordinate, gate, or hand off work."

const planDescriptionPi = "Plan and gate multi-agent work through punk-records over its HTTP API: create a coordination namespace, write /plan/summary, conventions and one /tasks fact per task with depends_on, leave a pointer in the repo namespace, hand workers a prompt, then gate with the task board, review each finished task, and release. Use when splitting a feature into tasks for other agents, or when asked to coordinate, gate, or hand off work."

var planTmpl = template.Must(template.New("plan").Funcs(template.FuncMap{
	"tool": func(o SkillOpts, name string) string { return "`" + ToolName(o.ToolPrefix, name) + "`" },
}).Parse(`---
name: punk-plan
description: {{.Description}}
{{- if .Opts.Hermes}}
version: 1.0.0
metadata:
  hermes:
    tags: [planning, coordination, punk]
    category: memory
{{- end}}
---
{{.Marker}}

# Punk Records planning

One session plans, other sessions build, and every fact they exchange lives in punk-records. The punk-memory skill is the worker side (find a task, claim it, report). This skill is the planner side. Both read the same facts, so a worker on any connected agent can pick up what you write here.
{{if .Opts.ServerURL}}
Server: {{.Opts.ServerURL}}. {{end}}{{if .Opts.Pi}}This agent reaches punk through four HTTP-backed tools ({{tool .Opts "whoami"}}, {{tool .Opts "recall"}}, {{tool .Opts "search"}}, {{tool .Opts "remember"}}) plus the HTTP API for the task board.{{else if .Opts.ToolPrefix}}Punk tools are prefixed ` + "`{{.Opts.ToolPrefix}}`" + ` in this agent.{{else}}Punk tools appear under their plain names (recall, remember, list_tasks, and so on) in this agent.{{end}}

## Before planning

- Write the brief and the plan as files in the repository first (a design brief, then a task-by-task plan with the code, tests and commit message for every task). Facts in punk point at those files; they do not replace them.
- Every task must end in something a reviewer can accept or reject on its own: a test that fails first, then passes, then one commit.
- Name the dependencies between tasks; that is what lets the board compute ` + "`ready`" + ` and ` + "`next`" + `.

## Set up the namespace

1. Choose a coordination namespace named after the project, ` + "`punk-<project>`" + ` (letters, digits, hyphens). Do not run a project in a repository's default namespace (the ` + "`agent-<repo>`" + ` one {{tool .Opts "whoami"}} returns from the repository directory): hooks write session noise there, and workers in other directories cannot resolve it.
2. {{if .Opts.Pi}}Call {{tool .Opts "whoami"}} to learn your identity.{{else}}Call {{tool .Opts "whoami"}}, then {{tool .Opts "register"}} in the new namespace with role ` + "`planner`" + `.{{end}}
3. Write these facts{{if .Opts.Pi}} with {{tool .Opts "remember"}}, one call each{{else}} with {{tool .Opts "remember_many"}}{{end}}, always passing the namespace explicitly:
   - ` + "`/plan/summary`" + ` (importance 0.9): the goal, the branch, the task ids in order and which may run in parallel, the paths of the brief, the plan and the worker prompt, who the planner is, and the hard rules.
   - ` + "`/conventions/repo`" + `: build, test and gate commands, commit rules (no attribution trailers, no long dashes if that is the house style), test helpers workers should reuse.
   - ` + "`/conventions/live-server`" + `: which punk server is the coordination server and that workers must never kill, restart or replace it; how to run a throwaway dev server on another port and stop it by its own pid.
   - one ` + "`/tasks/<id>`" + ` per task: the title on the first line, then a ` + "`files:`" + ` line, a ` + "`depends_on: A, B`" + ` line (or ` + "`none`" + `), what to build, the tests to run, and the commit message. Keep the full code in the plan file and name the plan section; a task fact is a pointer, not the plan.
4. Leave a pointer in the repository's default namespace, because a worker who forgets to pass the namespace will look there: ` + "`/plan/current`" + ` saying which namespace holds the work and that every call must pass it explicitly, and ` + "`/tasks/_where`" + ` with the same sentence so a task listing in the wrong namespace still points the right way.

## Hand off

Write a worker prompt to a file next to the plan and give that file to the workers. It carries, in this order: the namespace; the hard rule about the live server; setup (whoami, register, recall ` + "`/plan/summary`" + ` and the conventions, read the brief and the plan, check out the branch); how to pick a task ({{if .Opts.Pi}}` + "`GET /v1/namespaces/<ns>/tasks`" + `, take ` + "`next`" + `{{else}}{{tool .Opts "list_tasks"}}, take ` + "`next`" + ` or any ready row, {{tool .Opts "claim_work"}} on ` + "`/tasks/<id>`" + ` with a ttl that covers the work{{end}}, then recall ` + "`/tasks/<id>`" + ` for the text); the rules for doing it (failing test first, gate before commit, one commit per task with the plan's message, fix call sites to match the tree and record the deviation); how to finish ({{if .Opts.Pi}}` + "`POST /v1/namespaces/<ns>/tasks/<id>/status`" + ` with state done, the sha and the tests{{else}}{{tool .Opts "set_task_status"}} done with sha and tests, then {{tool .Opts "release_work"}}{{end}}); what to do when blocked (a precise question at ` + "`/questions/<id>`" + `, status blocked, move on, check ` + "`/answers/<id>`" + ` before retrying); and completion (the last worker writes ` + "`/plan/status`" + ` = ` + "`complete-<project>: <sha>`" + `; workers never merge, tag or release).

Tell workers to pass the namespace on every call. It is the most common reason a worker reports that it sees no tasks.

## Gate

- Wait for changes with {{if .Opts.Pi}}` + "`GET /v1/namespaces/<ns>/tasks?wait=55`" + `{{else}}{{tool .Opts "await_tasks"}} (timeout_seconds 55 unless the client allows longer){{end}} in a loop instead of polling on a timer. Every return carries the whole board; read it fresh, never a remembered key.
- Each newly done task: fetch the branch, run the gate commands from ` + "`/conventions/repo`" + ` against that commit, and review the diff against the plan and the brief. A real bug: write the fix at ` + "`/answers/<id>`" + ` and set the task's status to ` + "`review`" + ` with the issue; the worker re-claims it. A reviewer nit that changes nothing: leave the status alone and move on.
- Answer every ` + "`/questions/<id>`" + ` at ` + "`/answers/<id>`" + ` as soon as it appears; a blocked worker polls that key.
- Workers may be editing the very checkout you sit in. Review from the remote branch and run gates in a detached, throwaway worktree that you remove afterwards; never switch branches under a working worker.
- A claim whose lease expired, or a member whose ` + "`last_seen_at`" + ` is old, means the task is free again; say so at ` + "`/answers/<id>`" + ` so the next worker takes it.
- When ` + "`/plan/status`" + ` reads complete: run the full gate on the branch tip, run the acceptance checks from the brief on a dev server, merge from the primary checkout, tag and release, deploy, then write ` + "`/plan/review`" + ` (what was checked, what was rejected and why, the merge sha) and update ` + "`/plan/current`" + ` in the repository namespace to say no work is open.

## Facts the planner writes

| Key | Holds |
| --- | --- |
| ` + "`/plan/summary`" + ` | goal, branch, task order, file paths, hard rules |
| ` + "`/plan/current`" + ` (repo namespace) | pointer to the coordination namespace |
| ` + "`/conventions/repo`" + `, ` + "`/conventions/live-server`" + ` | gate commands, commit rules, server rules |
| ` + "`/tasks/<id>`" + ` | one task: title, files, depends_on, plan section, commit message |
| ` + "`/answers/<id>`" + ` | replies to worker questions and review verdicts |
| ` + "`/plan/review`" + ` | what the gate checked, the merge sha |

Never store secrets in any of them.
`))

// RenderPlanSkill renders the planner skill for one agent.
func RenderPlanSkill(o SkillOpts) string {
	var b strings.Builder
	desc := planDescription
	if o.Pi {
		desc = planDescriptionPi
	}
	_ = planTmpl.Execute(&b, map[string]any{
		"Opts":        o,
		"Description": desc,
		"Marker":      SkillMarker,
	})
	return b.String()
}

// Render picks the renderer for a target by its skill name.
func Render(tg SkillTarget) string {
	if tg.Name == PlanSkillName {
		return RenderPlanSkill(tg.Opts)
	}
	return RenderSkill(tg.Opts)
}
