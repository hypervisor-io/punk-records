# Codex 0.153.4 terminal/footer repetition investigation

Status: closed. Hypothesis 1 (upstream OSC 0 terminal-title stream)
CONFIRMED on the original host: the user ran one launch of
`codex -c 'tui.terminal_title=[]'` and the footer repetition was gone
everywhere (2026-09-06). The remediation in this document is the selected
fix; no Punk code change was required or made.

## Evidence base

- User input: `codex-spam.png` (repo root, untracked, user-owned; not
  committed). It shows the literal word `punkrecords` repeated horizontally
  after the transcript overlay's static `q to quit / esc to edit prev`
  footer.
- Host: WSL2 (`5.15.167.4-microsoft-standard-WSL2`), `codex-cli 0.153.4`,
  upstream `rust-v0.153.4` commit `3d2ee51ca2d5db578f328aa75e20aa22c0197c9a`.
  Experiments ran under tmux 3.2a with `TERM=xterm-256color`, window 200x50.
- Punk binary: built from the improvement worktree at integrated commit
  `444d1bd` (includes the C01 hook normalizer), run against a disposable
  dev server (sqlite, `ai.enabled=false`, `127.0.0.1:19393`,
  temp dir only, stopped afterwards). The real `~/.codex` config/hooks and
  the live `:9090` server were never modified; the temp CODEX_HOMEs carried
  only a copied `auth.json` (read-only on the real file, never printed,
  never written outside the temp dir) so the TUI could start.
- Raw PTY bytes were captured with `tmux pipe-pane` (every byte the TUI
  emitted, including escape sequences), grids with `capture-pane`.

## Four-cell A/B matrix (title on/off x Punk hooks on/off)

Fixture: a git directory named `punkrecords`; hooks-on cells used a temp
CODEX_HOME wired by `punk connect codex` against the dev server.

| Cell | Title | Hooks | OSC 0 bytes | Grid contamination | Footer bytes |
| ---- | ----- | ----- | ----------- | ------------------ | ------------ |
| 1 | on (default) | off | 11 sequences: `ESC ] 0 ; punkrecords BEL` and `ESC ] 0 ; <spinner> punkrecords BEL` | none | static hints only |
| 2 | off (`-c tui.terminal_title=[]`) | off | 0 | none | static hints only |
| 3 | on | on | 11 sequences, same shape | none; healthy context-only hooks hidden (no hook cells in transcript) | static hints only |
| 4 | off | on | 0 | none | static hints only |

Raw footer bytes (cell 1, end of stream):

```
... 100% ─ ESC[48;1H  ↑/↓ to scroll   pgup/pgdn to page   home/end to jump
ESC[49;1H q to quit   esc to edit prev ESC[39m ESC[49m ESC[0m ESC[?25l ESC[?2026l
```

Nothing follows the footer. Every `punkrecords` occurrence in the raw
stream is either inside an OSC 0 title sequence or the two legitimate grid
renders (the welcome card's `directory:` line and the composer status
line). No `hookSpecificOutput`/`additionalContext` bytes ever reach the
terminal stream in any cell - hook stdout is consumed by Codex, matching
upstream `hook_cell.rs` (successful context-only hooks are hidden) and
`pager_overlay.rs:870-895` (the footer is built from static shortcut hints
only; it has no memory/cwd/project text to corrupt).

Hooks-on cells also verified capture end-to-end through the C01
normalizer: `/agent-sessions/<sid>/start`, `/prompt-<native turn_id>`,
`/stop` all landed in the dev server; the prompt capture key is the native
turn UUID verbatim.

## Rename test

The fixture directory was copied to `renamedproj` and cell 1 repeated. The
OSC payloads follow the directory basename exactly: `ESC ] 0 ;
renamedproj BEL`, `ESC ] 0 ; <spinner> renamedproj BEL`. The repeated word
in the user's screenshot is the project directory basename.

## Hypotheses, ranked by evidence

1. **Upstream terminal-title stream leaking into the grid (CONFIRMED on the
   original host, 2026-09-06).** Upstream `codex-rs/tui/src/terminal_title.rs`
   (rust-v0.153.4) emits an OSC 0 window title with the default items
   activity + project-name (`chatwidget/status_surfaces.rs:27`), where
   project-name is the repository directory basename. Unchanged titles are
   deduplicated, but while the activity spinner rotates the title payload
   changes every frame (`punkrecords`, `⠋ punkrecords`, `⠙ punkrecords`,
   ...), so a busy session emits a steady stream of
   `ESC ] 0 ; <spinner> punkrecords BEL` sequences. In a terminal that
   consumes OSC 0 (tmux in these tests) none of this reaches the grid -
   confirmed clean in all four cells. In the user's renderer the payload
   painted at the cursor, producing the repeated literal `punkrecords`
   after the static footer. Confirmation: one original-host launch with
   the title items disabled removed the repetition everywhere; see
   "Original-host validation outcome" below.
   Source pins (rust-v0.153.4, commit 3d2ee51): the ONLY OSC 0 writer in
   the tree is `tui/src/terminal_title.rs:90` (`\x1b]0;{}\x07`), executed
   at `:66`/`:79`; its only callers are `status_surfaces.rs:268` (set) and
   `:235` (clear). The 100ms spinner scheduling lives at
   `status_surfaces.rs:30-34` and defeats the unchanged-title dedupe at
   `:264` while busy. `terminal_title=[]` early-returns at
   `status_surfaces.rs:256-261` before any write, so suppression is
   complete (fresh launch: zero title bytes; the only residual on an
   already-titled session is a single empty-title clear). Other
   terminal-control writers exist but none carry the project-name stream:
   alt-screen enter/leave `tui.rs:831-866`, sync-output `ESC[?2026`
   (`pager_overlay.rs:870-895`), status line (`tui.status_line`),
   OSC 9 notifications (`osc9.rs:50,52`, key `tui.notifications`),
   OSC 52 clipboard (`clipboard_copy.rs:487`), OSC 8 hyperlinks
   (`terminal_hyperlinks.rs:415`), cursor save/restore (`pets/mod.rs`,
   default off). Top-level `animations=false` stops the spinner but not
   static titles.
2. **Some other out-of-band or status pathway in the original host.**
   No additional mitigation was needed in the user's report. The exact
   original renderer was not identified, so its internal behavior was
   not independently traced.
3. **Punk hook output.** Not identified as the producer in the test matrix:
   hooks-off cells emit
   the identical OSC stream, hooks-on adds zero out-of-band bytes, no
   hook-specific envelope bytes appear in the terminal stream in any cell,
   and the installed hook entries carry no custom `statusMessage`
   (verified in the wired hooks.json: command/timeout/type/matcher only).
   No `suppressOutput` flag was added anywhere (PostToolUse's upstream
   output schema rejects it). The original-host fix disables an upstream
   Codex behavior only; all Punk hook capture and context injection paths
   are unchanged and remained fully functional with titles off (cell 4).

## Mitigation (verified in test cells AND on the original host)

Disable the terminal title items in Codex config:

```toml
[tui]
terminal_title = []
```

or one-shot per launch:

```
codex -c 'tui.terminal_title=[]'
```

Verified here: zero OSC title sequences (cells 2 and 4), with Punk hook
capture and context injection completely unaffected (cell 4 landed
start/prompt/stop facts with the title stream off). Verified on the
original host: one launch with titles disabled removed the visible footer
repetition everywhere (user-confirmed 2026-09-06). Persistence of the
setting across future launches was not separately verified. Users who
want a title without the spinner
re-emission can keep only the static project name:
`terminal_title = ["project-name"]` removes the per-frame activity prefix
(payload then dedupes to a single emission). That variant was NOT
exercised in these runs.

## Original-host validation outcome

The reviewer verified the coordinator's completed question response at
2026-09-06 12:20:41 UTC. The user answered "I fixed it with the
tui.terminal_title" and selected "Gone everywhere" for the question
about the affected host. This is user validation of the mitigation;
the four-cell matrix above is the separate instrumented test evidence.

1. Terminal identity: not re-collected; the user resolved the symptom
   directly with the title setting before supplying the terminal/app
   name. The confirmation below stands on its own because it is an A/B on
   the affected host.
2. One launch of `codex -c 'tui.terminal_title=[]'` on the original host:
   repetition after the transcript-overlay footer GONE everywhere
   (user-confirmed 2026-09-06). Per the pre-registered decision tree this
   confirms hypothesis 1 and closes the investigation as a configuration
   fix for an upstream behavior; no Punk code change.

## Related but separate confirmed defects (not the screenshot's cause)

- Native Codex UserPromptSubmit carries `turn_id` and no `prompt_id`;
  fixed by C01 (`fix(codex): normalize native hook event identifiers`).
- Duplicate managed skills across `~/.agents/skills` and
  `~/.codex/skills`; handled by C03.
- Session MCP fallback namespace (`agent-default`) vs checkout namespace;
  handled by C05.

## C06: repeatable integration acceptance procedure (2026-09-07)

Task C06 gates the complete Codex 0.153.4 integration. It has two halves
that must never be conflated:

- **SIMULATED** (`internal/api/codex_roundtrip_test.go` and the codex
  tests in `internal/hookcli`): drives the real server and the real hook
  CLI code in-process over the recorded native 0.153.4 fixtures
  (`internal/hookcli/testdata/codex-0.153.4/`, mirroring upstream
  codex-rs/hooks/src/schema.rs at rust-v0.153.4, commit
  `3d2ee51ca2d5db578f328aa75e20aa22c0197c9a`). These prove the
  hook-to-capture-to-injection lifecycle contracts; they say NOTHING
  about the TUI renderer.
- **NATIVE**: a real `codex` binary run (below). Only this observes TUI
  behavior (OSC title stream, footer, hook-status surfaces). The two
  halves are reported separately, and neither is ever cited as evidence
  for the other.

### Native procedure (repeatable)

Run entirely under `/tmp`; never touches the live `:9090` server, the
real `~/.codex` directory or any installed Punk file. The only live-file
read is one `cp` of `auth.json` into the temp CODEX_HOME (read-only on
the source, same boundary the C02 matrix used) so the TUI can start; the
file is never printed and never written back.

```sh
# 1. Build punk from the checkout under test.
mkdir -p /tmp/codex-c06/{home,proj,db}
go build -o /tmp/codex-c06/punk ./cmd/punk

# 2. Temp server: its own sqlite DB, its own port, AI off.
git init /tmp/codex-c06/proj
cat > /tmp/codex-c06/config.yaml <<'EOF'
http: { addr: 127.0.0.1:19397 }
db:   { driver: sqlite, dsn: /tmp/codex-c06/db/punk.db }
ai:   { enabled: false }
EOF
/tmp/codex-c06/punk migrate --config /tmp/codex-c06/config.yaml up   # flag BEFORE the action word
/tmp/codex-c06/punk serve   --config /tmp/codex-c06/config.yaml &    # stop this PID afterwards

# 3. Wire a temp CODEX_HOME (global scope; hooks derive the namespace
#    from the hook payload's cwd).
CODEX_HOME=/tmp/codex-c06/home /tmp/codex-c06/punk connect codex \
  --url http://127.0.0.1:19397
printf '\n[projects."/tmp/codex-c06/proj"]\ntrust_level = "trusted"\n' \
  >> /tmp/codex-c06/home/config.toml
cp ~/.codex/auth.json /tmp/codex-c06/home/auth.json   # read-only on the source

# 4. Run the real TUI under tmux with every byte captured.
tmux new-session -d -s c06 -x 200 -y 50 \
  'cd /tmp/codex-c06/proj && env CODEX_HOME=/tmp/codex-c06/home codex'
tmux pipe-pane -t c06 -o 'cat >> /tmp/codex-c06/raw-title-on.log'
#    Accept the "Hooks need review" prompt once (Trust all and continue),
#    send one short prompt, wait for the turn to finish.
#    Record the session ID from the /start capture key, quit, then
#    `codex resume <that-id>` AND SUBMIT another short prompt and wait
#    for that turn to finish: upstream queues the resume SessionStart
#    hook at reopen and dispatches it at the resumed session's next turn
#    (InitialHistory::Resumed queues SessionStartSource::Resume at
#    codex-rs/core/src/session/session.rs:1600,1623; dispatch from
#    run_pending_session_start_hooks at codex-rs/core/src/session/turn.rs:264 in rust-v0.153.4). Reopening without a turn fires
#    nothing - that is the upstream design, not a punk defect. Seed one
#    durable fact in the namespace BEFORE the resumed turn (an empty
#    session-start block is delivered as nothing and records no marker,
#    so the dispatch would be unobservable).

# 5. Observations against the temp server + raw capture:
#    - capture: GET :19397/v1/namespaces/agent-<dir-basename>/memories?prefix=/agent-sessions
#      expects /start, /prompt-<native turn uuid>, /tool-<id>, /stop
#      (note: the /start body is identical for startup and resume -
#      the translated envelope hardcodes source to the agent identity
#      "codex" by C01 design - so the store dedupes them to one fact;
#      the /delivery marker's event field is the startup/resume
#      discriminator)
#    - injection bookkeeping: /delivery ("<event> <rev> issued") and
#      /injected on the same session (C04 markers); after the resumed
#      turn, /delivery must read `resume <64-hex> issued` for the SAME
#      session ID
#    - title stream: grep the raw log for `ESC ] 0 ; ... BEL`
#    - duplicate delivery: replay the identical native SessionStart
#      payload (startup, and resume with source="resume" after the
#      resumed turn) through the installed hook command
#      (/tmp/codex-c06/punk hook --url http://127.0.0.1:19397 --from codex
#      < payload.json); expect exit 0, silent stdout, and the /delivery
#      marker byte-unchanged (same created_at) on every replay
#    - A/B: relaunch with `codex -c 'tui.terminal_title=[]'` in a fresh
#      tmux session; expect ZERO title sequences and unchanged captures.

# 6. Cleanup: kill the tmux session and the temp serve PID only.
```

### Native run record (2026-09-07, this procedure executed)

Environment: linux host, `codex-cli 0.153.4` (upstream rust-v0.153.4,
commit `3d2ee51ca2d5db578f328aa75e20aa22c0197c9a`), tmux 3.2a
200x50, temp Punk server on `127.0.0.1:19397` built from the C06
worktree at `9b65f9a` (all C01-C05/C07 integrated). Fixture directory
basename: `proj` (so any contamination would carry a distinct word from
the user's `punkrecords` report).

| Check | Title on (default) | Title off (`tui.terminal_title=[]`) |
| --- | --- | --- |
| OSC 0 title sequences in the raw TUI stream | 128 over the session, `ESC ] 0 ; proj BEL` and `ESC ] 0 ; <spinner> proj BEL` (spinner re-emission defeats the unchanged-title dedupe; payload is the directory basename, matching C02's rename test) | 0 |
| Grid contamination under tmux | none (tmux consumes OSC 0) | none |
| Native TUI hook capture | SessionStart `/start` (`source=codex`), UserPromptSubmit `/prompt-<native turn uuid verbatim>`, PostToolUse `/tool-<id>` (Bash command + output), Stop `/stop` (assistant message) - all in the cwd-derived `agent-proj` namespace | identical: `/start`, `/prompt-`, `/delivery`, `/injected`, `/stop` captured with titles off (C02 matrix cell 4 reconfirmed on this host) |
| Context injection | C04 bookkeeping (`/delivery` = `startup <64-hex> issued`, `/injected`) written during the TUI's session-start fetch - the hook fetched and the server issued a non-empty block; successful context-only hooks are hidden by the TUI (upstream hook_cell.rs), so the bookkeeping facts are the server-side evidence | same as title-on |
| Duplicate delivery | replaying the identical native SessionStart payload twice through the installed hook command after the TUI session's own startup: silent both times (C04 marker suppression), exit 0 both times | - |
| Foreign/hook trust | first launch shows the "Hooks need review" gate; trusting once activates all four wired events for the session and later launches | same |

### Resume dispatch verification (2026-09-07, revision 1 run)

The first C06 run observed `codex resume --last` firing no SessionStart
hook and recorded that as a limit. That was an experiment design error,
not a Codex behavior: reopening alone cannot dispatch the hook. Pinned
rust-v0.153.4 source (commit `3d2ee51ca2d5db578f328aa75e20aa22c0197c9a`)
shows `InitialHistory::Resumed` QUEUES `SessionStartSource::Resume`
(`codex-rs/core/src/session/session.rs:1600,1623`) and dispatches it
from `run_pending_session_start_hooks` when the resumed session's first
turn starts (`codex-rs/core/src/session/turn.rs:264`). The revision-1 run below reproduced the
firing under that design.

Isolated setup identical to the run above (fresh temp CODEX_HOME, fresh
temp Punk server on `127.0.0.1:19398`, fresh sqlite DB, same fixture
directory, same binary). Timeline (all times UTC 2026-09-07, one session
throughout, namespace `agent-proj`):

| Time | Event |
| --- | --- |
| 11:03:30.967 | TUI launch, turn 1 submitted ("reply with the single word ready"): `/agent-sessions/01a07b89-43aa-7023-9980-5ddc6f4ee692/start` captured (`cwd=/tmp/codex-c06/proj source=codex`) |
| 11:03:30.986 | `/prompt-01a07b89-9897-...` captured (native turn UUID verbatim) |
| 11:03:34.374 | `/stop` captured; turn 1 complete |
| 11:04:1x | session quit; `codex resume 01a07b89-43aa-7023-9980-5ddc6f4ee692` reopened the same conversation. NO hook dispatch at reopen (consistent with the queued-dispatch design). Turn 2 submitted ("reply with the single word resumed"): `/prompt-01a07b8a-bb9a-...` at 11:04:45, `/stop` at 11:04:49, but no `/delivery` - the namespace held no durable facts yet, so the session-start block assembled EMPTY and a delivery of nothing records no marker |
| 11:05:4x | one durable fact seeded via REST (`/decisions/gate`); session quit; `codex resume 01a07b89-43aa-7023-9980-5ddc6f4ee692` again; turn 3 submitted |
| 11:05:58.691 | **`/agent-sessions/01a07b89-43aa-7023-9980-5ddc6f4ee692/delivery` body `resume c66ad8db5070580bf37eab253e9bd600e9cc42510abb0dfd939ee9d5dccd27d9 issued`** - the resume SessionStart hook DISPATCHED at the resumed turn and fetched context with `event=resume`, same session ID |
| 11:05:58.695 | `/injected` recorded with the seeded fact ID - the server issued a non-empty resume block through the native hook; this bookkeeping does not acknowledge host consumption |
| 11:05:58.715 | `/prompt-01a07b8b-d995-...` captured; `/stop` at 11:06:01; turn 3 complete |

Duplicate-delivery replay: the native resume SessionStart payload for
that exact session (`source="resume"`, same session_id/cwd) replayed
twice through the installed hook command
(`/tmp/codex-c06/punk hook --url http://127.0.0.1:19398 --from codex`):
both replays exited 0 with silent stdout and the `/delivery` marker was
byte-unchanged (same `created_at` 11:05:58.691, same body) - the C04
once-per-(event, revision) suppression held on both replays.

Capture-key caveat, recorded precisely: a resume `/start` capture is
indistinguishable from the startup one in the store - the C01
translator hardcodes the translated envelope's source to the agent
identity `codex` (never the session-start reason), so both events write
the identical body at `/agent-sessions/<sid>/start` and the store
dedupes them to a single fact. The delivery marker's event field is the
startup/resume discriminator, and the replayed-payload silence plus the
`event=resume` marker verify native resume dispatch and dedupe at the
server-issued boundary; they do not prove that the host consumed the
context. Where native event identity is absent, the documented bounded
resume policy applies. The simulated resume contract remains
covered by `TestAgentContextResumeRefreshesOnce`,
`TestRunFromCodexResumeInjectsOncePerEvent`, and the `resume refreshes
once per resume event` lifecycle subtest in
`internal/api/codex_roundtrip_test.go`.

This run independently confirmed repeated OSC 0 title emission and its
suppression with `[tui] terminal_title = []`, while hooks remained
functional. It did not reproduce visible contamination: tmux consumed
the title sequences. The original user confirmed that disabling titles
stopped the visible spam; the exact renderer behavior on that host
remains unisolated.
