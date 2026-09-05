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
