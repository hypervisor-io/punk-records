package hookcli

// openCodeBridgeExtrasJS is the OpenCode-only splice the generated plugin
// embeds after the shared inbox bridge core (inbox_renderjs.go): delivery
// diagnostics reporting and the restart-persistent recovery state. It is
// deliberately NOT shared with the pi/OpenClaw bridges - their hosts have
// no SDK client prompt path and no process-restart semantics like
// OpenCode's, so their delivery observations and recovery needs differ
// (subprocess status reporting is planned as separate work).
//
// Plain raw-string constant: no fmt verbs are substituted into it, but it
// is spliced into openCodePluginTemplate as a Sprintf ARGUMENT and must
// survive being read as Go source, so like inboxBridgeCoreJS it carries no
// backticks (raw-string delimiters) and no literal percent characters
// (kept out of any future Sprintf-of-the-splice footgun), no template
// literals and no U+2028/U+2029. It expects the host template to already
// define punkFetch(path, init) (bounded, never-rejecting,
// parsed-JSON-or-null), messagingEnabled, punkSessions, punkNamespaceCache,
// punkServerURL, fnv1aHex, and the shared inbox machinery
// (punkInboxEnvInt, punkInboxStAlive, PUNK_INBOX_WAKE_WINDOW_MS_DEFAULT,
// PUNK_INBOX_WAKE_MAX_DEFAULT, punkInboxLeaseMs). Every identifier it
// declares is punkDiag*- or punkRecovery*-prefixed so it cannot collide
// with host-template helpers.
//
// Two behaviors, both opt-in-gated to zero effect when PUNK_MESSAGING is
// not "1":
//
//   - DIAGNOSTICS (docs/superpowers/specs/2026-09-28-messaging-reliability
//     -design.md, "Shared HTTP contract"): one bounded, deduplicated
//     observation per meaningful transition, POSTed to
//     /v1/namespaces/<ns>/messages/diagnostics. A diagnostic describes a
//     BRIDGE OBSERVATION - it never proves the model received or completed
//     anything. Old servers without the route answer 404; punkFetch
//     collapses that to null and delivery is untouched. Reports are
//     SERIALIZED per session with latest-only coalescing: at most one POST
//     is in flight and at most one newer snapshot waits behind it, so a
//     slow first POST can never complete after a newer observation and
//     leave the server holding stale state. Identical consecutive
//     snapshots are suppressed for PUNK_DIAG_DEDUP_MS so hot loops (SSE
//     reconnects, retry ladders, hint storms draining an empty backlog)
//     cannot spam the endpoint; a CHANGED snapshot always flushes. Counts
//     are clamped nonnegative and bounded; timestamps are RFC3339
//     (Date.toISOString, quantized to the second so a re-computed retry
//     time does not dodge the dedup); last_error is a short machine
//     reason, so no URLs, bodies or raw exceptions leave the process.
//     Reporting never blocks delivery: callers fire and forget.
//
//   - RECOVERY: the ids of messages successfully handed to
//     client.session.prompt whose ACK is not yet confirmed, plus the
//     sliding-window wake timestamps, persisted in one small JSON file per
//     (server, namespace, address) under
//     $XDG_STATE_HOME/punk/inbox/opencode/<address-slug>-<hash>.json -
//     mirroring hookcli's own inboxStatePath naming so the two live side
//     by side. Never credentials, never message bodies. Untrusted input is
//     bounded on every axis: the file is rejected outright above
//     PUNK_RECOVERY_MAX_FILE_BYTES, restored pending ids must match the
//     server's own id syntax (exactly 32 lowercase hex chars,
//     region.newMessageID - every id this bridge ever persists came from a
//     server row, so anything else is not our evidence; this also keeps a
//     full-capacity record far inside the file bound), and wake stamps are
//     bounded on disk at PUNK_RECOVERY_MAX_WAKE_STAMPS (the newest) with
//     the older in-window ones CONSERVED as an overflow summary
//     {count, until} - restored as stamps that all expire at the
//     documented last expiry - so a configured cap above the stamp bound
//     still survives a restart without ever writing a file the restore
//     would reject. Writes are serialized per
//     session through a promise chain AND across processes through the
//     same O_EXCL .lock sibling convention as hookcli's updateInboxState,
//     and are atomic (temp file + rename, with the temp file cleaned up
//     if the rename fails). Cross-writer conflict policy, stated honestly:
//     a snapshot on disk whose saved_at is AHEAD of this process's clock
//     (within PUNK_RECOVERY_SKEW_MS; beyond it the record is treated as
//     skewed or forged and rewritten) is forward state - our writes skip
//     while it stays ahead, and our fresh handoffs are NOT persisted
//     during that time (memory-only until the other writer stops or this
//     process restarts; residual limitation, no lossless guarantee is
//     claimed for that window). A foreign record at or below our clock
//     may equally be a LIVE writer (writers share the host clock, so
//     recency proves nothing either way) - so instead of guessing, the
//     write MERGES conservatively: the foreign pending ids are adopted
//     into the delivered set and the wake evidence is unioned taking, per
//     identical timestamp, the MAX of the two records' multiplicities -
//     never the sum (evidence shared through a common restored lineage is
//     counted once) and never 1 (genuine same-millisecond wakes on either
//     side keep their count) - then a newer
//     snapshot is written. Two live writers ping-pong snapshots, but
//     every merge only ADDS evidence - no writer's handed-off ids or
//     spent wakes are silently dropped. Adopted ids reconcile through the
//     same trust-the-server path as restart restore: re-ack if the row
//     still exists, drop after the shared reack-absence limit if it does
//     not. Corrupt, missing or unwritable state
//     fails OPEN - in-memory delivery keeps working exactly as before,
//     with one generic log line (no paths, no payload). Restore runs once
//     per session, at confirmed registration, BEFORE any drain can run
//     (the host template gates drains on st.recoveryReady): restored
//     pending ids land in the delivered set so the first fetch pass treats
//     them as re-acks (never re-prompts), and restored wake stamps already
//     count against the cap. A crash between the host handoff and the
//     state save remains ambiguous - at-least-once semantics, never
//     claimed as exactly-once.
//
// PUNK_MESSAGING_RESTORE_DELAY_MS is a TEST-ONLY pacing knob (same family
// as PUNK_MESSAGING_BACKOFF_MS): it holds punkRecoveryRestore open so
// behavioral tests can race idle transitions and inbox hints against the
// restore window deterministically.
const openCodeBridgeExtrasJS = `
  // ---- punk delivery diagnostics + restart recovery (OpenCode bridge) ----
  const PUNK_RECOVERY_PENDING_CAP = 250 // the fetch pass bound (5 rounds x 50 rows)
  const PUNK_RECOVERY_MAX_FILE_BYTES = 65536 // untrusted file bound; larger records are rejected and rewritten
  const PUNK_RECOVERY_MAX_WAKE_STAMPS = 1000 // wake stamps PERSISTED per record (the newest); older in-window ones become the overflow summary
  const PUNK_RECOVERY_MAX_OVERFLOW = 10000 // sanity bound on a persisted overflow count (hostile-file defense)
  const PUNK_RECOVERY_SKEW_MS = 60000 // saved_at further ahead than this is skewed/forged, not a live writer
  const PUNK_DIAG_DEDUP_MS = 60000

  let punkRecoveryFsMod = null
  let punkRecoveryCryptoMod = null
  let punkRecoveryOsMod = null
  let punkRecoveryModulesPromise = null

  // punkRecoveryModules loads node:fs / node:crypto / node:os exactly once
  // per plugin life through ONE shared promise. Concurrent sessions
  // registering in the same tick all await the same initialization, so no
  // caller can observe a half-initialized module set (a latched boolean
  // let a second caller return early with fs still null - its restore
  // silently skipped, re-prompting an already-handed-off id - or with
  // crypto missing, persisting under a fallback-hash filename the next
  // properly-initialized life would never read back). Dynamic import()
  // rather than require() because this plugin executes as an ES module
  // under Node (require is undefined there) and under Bun (where import()
  // also works); the shared punkInboxOwner's require-with-fallback
  // documents the same constraint from the other direction. Only recovery
  // code reaches this, so a messaging-disabled plugin never loads a module
  // at all. A failed load resolves the shared promise false forever - fail
  // open, in-memory delivery continues without retrying the import on
  // every save.
  function punkRecoveryModules() {
    if (!punkRecoveryModulesPromise) {
      punkRecoveryModulesPromise = (async () => {
        try {
          punkRecoveryFsMod = await import("node:fs")
        } catch (err) {
          punkRecoveryFsMod = null
        }
        try {
          punkRecoveryCryptoMod = await import("node:crypto")
        } catch (err) {
          punkRecoveryCryptoMod = null
        }
        try {
          punkRecoveryOsMod = await import("node:os")
        } catch (err) {
          punkRecoveryOsMod = null
        }
        return punkRecoveryFsMod !== null
      })()
    }
    return punkRecoveryModulesPromise
  }

  // State root mirrors hookcli's inboxStateRoot (inbox_state.go):
  // $XDG_STATE_HOME when absolute, else ~/.local/state, else the OS temp
  // dir. Empty means recovery is off for this host (fail open).
  function punkRecoveryRoot() {
    try {
      const x = typeof process !== "undefined" && process.env && process.env.XDG_STATE_HOME
      if (x && /^(?:\/|[A-Za-z]:[\\/])/.test(x)) return x + "/punk/inbox/opencode"
      const home = typeof process !== "undefined" && process.env && process.env.HOME
      if (home) return home + "/.local/state/punk/inbox/opencode"
      if (punkRecoveryOsMod && typeof punkRecoveryOsMod.tmpdir === "function") {
        return punkRecoveryOsMod.tmpdir() + "/punk-state/inbox/opencode"
      }
    } catch (err) {}
    return ""
  }

  // punkRecoverySlug mirrors inboxSafe: filename-safe, never "." or "..".
  function punkRecoverySlug(s, max) {
    let out = String(s).replace(/[^A-Za-z0-9_-]+/g, "_")
    if (out.length > max) out = out.slice(0, max)
    if (out === "") out = "_"
    return out
  }

  // punkRecoveryFile mirrors inboxStatePath: a readable address slug, but
  // the hash of server+namespace+address is what isolates the file - the
  // same address against two servers or two namespaces never shares
  // recovery state. sha256 via node:crypto; without crypto the fnv1a
  // fallback only degrades the filename's collision resistance, and the
  // identity fields inside the record make a collision self-rejecting on
  // load (punkRecoveryRestore's identity guard).
  function punkRecoveryFile(ns, agent) {
    const root = punkRecoveryRoot()
    if (!root) return ""
    const joined = punkServerURL() + "\u0000" + ns + "\u0000" + agent
    let hash = ""
    if (punkRecoveryCryptoMod && typeof punkRecoveryCryptoMod.createHash === "function") {
      try {
        hash = punkRecoveryCryptoMod.createHash("sha256").update(joined).digest("hex").slice(0, 16)
      } catch (err) {
        hash = ""
      }
    }
    if (!hash) hash = fnv1aHex(joined) + fnv1aHex("2" + joined)
    return root + "/" + punkRecoverySlug(agent, 48) + "-" + hash + ".json"
  }

  // punkRecoveryValidId: a restorable pending id must match the server's
  // own id syntax EXACTLY - region.newMessageID mints every message id as
  // 32 lowercase hex chars, and every id this bridge persists came from a
  // server row, so anything else in a state file is not our evidence.
  // Restricting to the real syntax also keeps a full-capacity record
  // (250 ids x 34 bytes + 1000 wake stamps + metadata) far inside the
  // 64 KiB file bound, where a generic 256-byte id allowance could bust
  // it and make the restore reject the bridge's own record.
  function punkRecoveryValidId(v) {
    if (typeof v !== "string" || v.length !== 32) return false
    for (let i = 0; i < 32; i++) {
      const c = v.charCodeAt(i)
      if (!((c >= 48 && c <= 57) || (c >= 97 && c <= 102))) return false
    }
    return true
  }

  // punkRecoveryRestore loads the session's recovery record ONCE (the
  // punkRestored latch) and merges it into the live state: pending ids
  // into the delivered set (so the next fetch pass re-acks them instead of
  // re-prompting - the restart contract) and unexpired wake stamps into
  // the sliding window (so the wake budget survives the restart). The
  // untrusted file is bounded on every axis: rejected outright above
  // PUNK_RECOVERY_MAX_FILE_BYTES (stat before read, and the raw length
  // re-checked after), pending ids validated against the server's own id
  // syntax one by one, wake stamps validated and bounded to the newest
  // PUNK_RECOVERY_MAX_WAKE_STAMPS plus the persisted overflow summary
  // (conserved as stamps expiring at the documented last expiry, count
  // itself bounded). A stamp from a skewed future clock is tolerated up
  // to a minute, never beyond the window. Missing, corrupt, oversized,
  // mismatched-identity or unreadable state restores nothing and never
  // throws - fail open; the next save rewrites the file cleanly. Returns
  // how many ids were restored.
  async function punkRecoveryRestore(ns, st) {
    try {
      if (!messagingEnabled || !st || st.punkRestored) return 0
      st.punkRestored = true
      if (!(await punkRecoveryModules())) return 0
      // TEST-ONLY pacing knob: hold the restore window open so tests can
      // race idle transitions and hints against it (see the Go doc
      // comment). Zero in production. Cancellable through the shared
      // sleep so a session deleted or a plugin disposed mid-restore does
      // not wait the delay out.
      const delayMs = punkInboxEnvInt("PUNK_MESSAGING_RESTORE_DELAY_MS", 0)
      if (delayMs > 0) await punkInboxCancellableSleep(st, delayMs)
      const file = punkRecoveryFile(ns, st.agent)
      if (!file) return 0
      let size = 0
      try {
        const stt = await punkRecoveryFsMod.promises.stat(file)
        size = stt.size
      } catch (err) {
        return 0
      }
      if (size > PUNK_RECOVERY_MAX_FILE_BYTES) return 0
      let raw = ""
      try {
        raw = await punkRecoveryFsMod.promises.readFile(file, "utf8")
      } catch (err) {
        return 0
      }
      if (raw.length > PUNK_RECOVERY_MAX_FILE_BYTES) return 0
      const rec = JSON.parse(raw)
      if (!rec || typeof rec !== "object") return 0
      if (rec.server !== punkServerURL() || rec.namespace !== ns || rec.address !== st.agent) return 0
      const now = Date.now()
      const windowMs =
        punkInboxEnvInt("PUNK_MESSAGING_CONTINUE_WINDOW_SECONDS", PUNK_INBOX_WAKE_WINDOW_MS_DEFAULT / 1000) * 1000
      // Wake evidence, bounded: keep the NEWEST PUNK_RECOVERY_MAX_WAKE_
      // STAMPS stamps, and conserve older in-window ones through the
      // persisted overflow summary - restored as stamps that all expire
      // at the summary's documented last expiry (the latest instant any
      // dropped stamp could still count), so the restored budget never
      // under-counts while the overflow is live. The overflow count is
      // itself bounded (hostile-file defense).
      const kept = []
      if (Array.isArray(rec.wake_times)) {
        for (let i = 0; i < rec.wake_times.length; i++) {
          const t = rec.wake_times[i]
          if (typeof t === "number" && t > 0 && t <= now + 60000 && now - t < windowMs) kept.push(t)
        }
      }
      const newest =
        kept.length > PUNK_RECOVERY_MAX_WAKE_STAMPS ? kept.slice(kept.length - PUNK_RECOVERY_MAX_WAKE_STAMPS) : kept
      const syn = []
      if (
        rec.wake_overflow &&
        typeof rec.wake_overflow === "object" &&
        typeof rec.wake_overflow.count === "number" &&
        typeof rec.wake_overflow.until === "number" &&
        rec.wake_overflow.count > 0 &&
        rec.wake_overflow.count <= PUNK_RECOVERY_MAX_OVERFLOW &&
        rec.wake_overflow.until > now &&
        rec.wake_overflow.until - windowMs > 0
      ) {
        const t = rec.wake_overflow.until - windowMs
        const n = Math.min(rec.wake_overflow.count, PUNK_RECOVERY_MAX_OVERFLOW)
        for (let i = 0; i < n; i++) syn.push(t)
      }
      const combined = syn.concat(newest)
      combined.sort(function (a, b) { return a - b })
      for (let i = 0; i < combined.length; i++) st.wakeTimes.push(combined[i])
      let restored = 0
      if (Array.isArray(rec.pending_ids)) {
        for (let i = 0; i < rec.pending_ids.length && st.delivered.size < PUNK_RECOVERY_PENDING_CAP; i++) {
          const id = rec.pending_ids[i]
          if (punkRecoveryValidId(id) && !st.delivered.has(id)) {
            st.delivered.add(id)
            restored++
          }
        }
      }
      st.punkSavedAt = typeof rec.saved_at === "number" && rec.saved_at > 0 ? rec.saved_at : 0
      return restored
    } catch (err) {
      return 0
    }
  }

  // punkRecoveryLock / punkRecoveryUnlock implement the same cross-process
  // state-lock convention as hookcli's updateInboxState (inbox_state.go):
  // an O_EXCL-created sibling .lock file, a 10-second staleness cleanup (a
  // crashed holder never wedges the state forever), and a bounded 2-second
  // acquisition wait. Without it the saved_at guard below is a TOCTOU: an
  // external writer landing between the guard's read and the rename would
  // be clobbered anyway. Lock timeout (another writer is active) skips the
  // write - fail open, in-memory delivery continues, at-least-once.
  async function punkRecoveryLock(file) {
    const lock = file + ".lock"
    const deadline = Date.now() + 2000
    for (;;) {
      try {
        const fh = await punkRecoveryFsMod.promises.open(lock, "wx")
        await fh.close()
        return true
      } catch (err) {
        if (!err || err.code !== "EEXIST") return false
      }
      try {
        const st = await punkRecoveryFsMod.promises.stat(lock)
        if (Date.now() - st.mtimeMs > 10000) {
          try {
            await punkRecoveryFsMod.promises.unlink(lock)
          } catch (err2) {}
          continue
        }
      } catch (err) {}
      if (Date.now() > deadline) return false
      await new Promise((resolve) => setTimeout(resolve, 25))
    }
  }

  async function punkRecoveryUnlock(file) {
    try {
      await punkRecoveryFsMod.promises.unlink(file + ".lock")
    } catch (err) {}
  }

  // punkRecoverySave persists the session's recovery snapshot. Serialized
  // per session through a promise chain (st.punkRecoveryChain) AND across
  // processes through the sibling .lock file, so overlapping writers can
  // never interleave with the conflict check; atomic via temp file +
  // rename, with the temp file unlinked if the rename fails; bounded
  // (pending ids capped, wake stamps pruned to the current window and
  // persisted as the newest PUNK_RECOVERY_MAX_WAKE_STAMPS plus an overflow
  // summary for the older in-window ones, so a high configured cap
  // survives a restart without ever writing a file the restore would
  // reject). Conflict policy against what another process left on disk:
  //   - our own last record (saved_at matches st.punkSavedAt): overwrite.
  //   - a foreign record AHEAD of our clock, within PUNK_RECOVERY_SKEW_MS:
  //     forward state - SKIP this write entirely. While it stays ahead, our
  //     fresh handoffs are memory-only (residual limitation, stated in the
  //     Go doc comment; no lossless guarantee is claimed for that window).
  //   - a foreign record further ahead than the skew tolerance: skewed or
  //     forged - ignore it and rewrite.
  //   - any OTHER foreign record: its writer may be live or gone - writers
  //     share the host clock, so recency cannot tell. MERGE conservatively
  //     instead of guessing: adopt its identity-matched pending ids into
  //     the delivered set and union its wake evidence with ours taking,
  //     per identical timestamp, the MAX of the two records'
  //     multiplicities - never the sum (evidence shared through a common
  //     restored lineage is counted once) and never 1 (genuine
  //     same-millisecond wakes on either side keep their count) - then
  //     write a newer snapshot. Every merge only ADDS evidence; adopted
  //     ids reconcile through the shared reack machinery (re-ack if the
  //     row still exists, drop after the reack-absence limit if it does
  //     not).
  //     An identity-MISMATCHED record is never merged - it is not evidence
  //     for this identity (filename collision or stale rename).
  // The returned promise never rejects; a failed write latches
  // punkRecoveryBroken with one generic log line and delivery continues
  // purely in memory.
  function punkRecoverySave(ns, st) {
    try {
      if (!messagingEnabled || !st || st.punkRecoveryBroken) return Promise.resolve()
      const run = async () => {
        try {
          if (st.punkRecoveryBroken || !punkInboxStAlive(st)) return
          if (!(await punkRecoveryModules())) return
          const file = punkRecoveryFile(ns, st.agent)
          if (!file) return
          const dir = file.slice(0, file.lastIndexOf("/"))
          await punkRecoveryFsMod.promises.mkdir(dir, { recursive: true })
          if (!(await punkRecoveryLock(file))) return
          try {
            let cur = null
            try {
              const stt = await punkRecoveryFsMod.promises.stat(file)
              if (stt.size <= PUNK_RECOVERY_MAX_FILE_BYTES) {
                const raw = await punkRecoveryFsMod.promises.readFile(file, "utf8")
                if (raw.length <= PUNK_RECOVERY_MAX_FILE_BYTES) {
                  const parsed = JSON.parse(raw)
                  if (parsed && typeof parsed === "object" && typeof parsed.saved_at === "number") cur = parsed
                }
              }
            } catch (err) {
              cur = null
            }
            const now = Date.now()
            const windowMs =
              punkInboxEnvInt("PUNK_MESSAGING_CONTINUE_WINDOW_SECONDS", PUNK_INBOX_WAKE_WINDOW_MS_DEFAULT / 1000) * 1000
            const own = cur !== null && cur.saved_at === (st.punkSavedAt || 0)
            if (cur !== null && !own) {
              if (cur.saved_at > now + PUNK_RECOVERY_SKEW_MS) {
                // Skewed or forged saved_at: not a plausible writer on
                // this host (state homes are per-machine, so a real
                // concurrent writer shares our clock). Ignore - no merge.
                cur = null
              } else if (cur.saved_at > now) {
                // Forward state from a writer ahead of our clock: never
                // clobber it with our older-knowledge snapshot.
                return
              } else if (cur.server === punkServerURL() && cur.namespace === ns && cur.address === st.agent) {
                // A foreign record at or below our clock: its writer may
                // be live or gone (writers share the host clock, so
                // recency proves nothing either way). Merge conservatively
                // instead of guessing - adopt its still-pending ids and
                // union its wake evidence - then write a newer snapshot.
                if (Array.isArray(cur.pending_ids)) {
                  for (let i = 0; i < cur.pending_ids.length && st.delivered.size < PUNK_RECOVERY_PENDING_CAP; i++) {
                    const id = cur.pending_ids[i]
                    if (punkRecoveryValidId(id) && !st.delivered.has(id)) st.delivered.add(id)
                  }
                }
                const wakeCounts = new Map()
                for (let i = 0; i < st.wakeTimes.length; i++) {
                  const t = st.wakeTimes[i]
                  if (t > 0 && now - t < windowMs) wakeCounts.set(t, (wakeCounts.get(t) || 0) + 1)
                }
                if (Array.isArray(cur.wake_times)) {
                  // Count the foreign record's per-timestamp multiplicities
                  // FIRST, then merge with max: for an identical timestamp
                  // the merged evidence is max(ours, theirs) - never the
                  // sum (evidence shared through a common restored lineage
                  // is counted once) and never 1 (genuine same-millisecond
                  // wakes on the foreign side keep their count).
                  const foreignCounts = new Map()
                  for (let i = 0; i < cur.wake_times.length; i++) {
                    const t = cur.wake_times[i]
                    if (typeof t === "number" && t > 0 && t <= now + 60000 && now - t < windowMs) {
                      foreignCounts.set(t, (foreignCounts.get(t) || 0) + 1)
                    }
                  }
                  const foreignTimes = Array.from(foreignCounts.keys())
                  for (let i = 0; i < foreignTimes.length; i++) {
                    const t = foreignTimes[i]
                    wakeCounts.set(t, Math.max(wakeCounts.get(t) || 0, foreignCounts.get(t)))
                  }
                }
                if (
                  cur.wake_overflow &&
                  typeof cur.wake_overflow === "object" &&
                  typeof cur.wake_overflow.count === "number" &&
                  typeof cur.wake_overflow.until === "number" &&
                  cur.wake_overflow.count > 0 &&
                  cur.wake_overflow.until > now &&
                  cur.wake_overflow.until - windowMs > 0
                ) {
                  const t = cur.wake_overflow.until - windowMs
                  const n = Math.min(cur.wake_overflow.count, PUNK_RECOVERY_MAX_OVERFLOW)
                  wakeCounts.set(t, Math.max(wakeCounts.get(t) || 0, n))
                }
                const merged = []
                const times = Array.from(wakeCounts.keys()).sort(function (a, b) { return a - b })
                for (let i = 0; i < times.length; i++) {
                  for (let j = 0; j < wakeCounts.get(times[i]); j++) merged.push(times[i])
                }
                st.wakeTimes = merged
              }
              // An identity-mismatched foreign record falls through
              // unmerged: it is not evidence for this identity.
            }
            st.wakeTimes = st.wakeTimes.filter((t) => t > 0 && now - t < windowMs)
            // Bound what is PERSISTED: the newest stamps within the bound,
            // older in-window ones conserved as an overflow summary whose
            // until is the instant every dropped stamp has expired - the
            // summary round-trips exactly through restore's synthesis, so
            // a configured cap above the stamp bound survives restarts.
            let wakeSave = st.wakeTimes
            let wakeOverflow = undefined
            if (wakeSave.length > PUNK_RECOVERY_MAX_WAKE_STAMPS) {
              const cut = wakeSave.length - PUNK_RECOVERY_MAX_WAKE_STAMPS
              wakeOverflow = { count: cut, until: wakeSave[cut - 1] + windowMs }
              wakeSave = wakeSave.slice(cut)
            }
            const rec = {
              server: punkServerURL(),
              namespace: ns,
              address: st.agent,
              pending_ids: Array.from(st.delivered).slice(0, PUNK_RECOVERY_PENDING_CAP),
              wake_times: wakeSave,
              saved_at: now,
            }
            if (wakeOverflow) rec.wake_overflow = wakeOverflow
            const tmp = file + ".tmp-" + Date.now() + "-" + Math.floor(Math.random() * 1000000000)
            let renamed = false
            try {
              await punkRecoveryFsMod.promises.writeFile(tmp, JSON.stringify(rec) + "\n", { mode: 384 })
              await punkRecoveryFsMod.promises.rename(tmp, file)
              renamed = true
            } finally {
              // A failed rename must not leave our temp file as garbage
              // next to the state file (the name is unique per write, so
              // it is unambiguously ours). After a successful rename the
              // path is gone already - skip the unlink.
              if (!renamed) {
                try {
                  await punkRecoveryFsMod.promises.unlink(tmp)
                } catch (err2) {}
              }
            }
            st.punkSavedAt = now
          } finally {
            await punkRecoveryUnlock(file)
          }
        } catch (err) {
          if (!st.punkRecoveryBroken) {
            st.punkRecoveryBroken = true
            console.error("punk connect opencode: recovery state write failed; continuing in memory")
          }
        }
      }
      const prev = st.punkRecoveryChain || Promise.resolve()
      const next = prev.then(run, run)
      st.punkRecoveryChain = next.catch(() => {})
      return next
    } catch (err) {
      return Promise.resolve()
    }
  }

  // punkDiagNamespace: the namespace a diagnostics POST targets - the
  // resolved cache (or the PUNK_NAMESPACE override) once registration
  // confirmed it. Empty means "not known yet": reports are skipped rather
  // than guessed at.
  function punkDiagNamespace() {
    if (punkNamespaceCache) return punkNamespaceCache
    const o = typeof process !== "undefined" && process.env && process.env.PUNK_NAMESPACE
    return o || ""
  }

  // punkDiagWindowWakeCount: the wake stamps still inside the sliding
  // window - the budget the operator sees.
  function punkDiagWindowWakeCount(st) {
    const now = Date.now()
    const windowMs =
      punkInboxEnvInt("PUNK_MESSAGING_CONTINUE_WINDOW_SECONDS", PUNK_INBOX_WAKE_WINDOW_MS_DEFAULT / 1000) * 1000
    let n = 0
    for (let i = 0; i < st.wakeTimes.length; i++) {
      if (now - st.wakeTimes[i] < windowMs) n++
    }
    return n
  }

  // punkDiagReport records one bounded observation for a REGISTERED
  // session and flushes observations to the server SERIALIZED, newest
  // last. At most one POST per session is in flight and at most one newer
  // snapshot waits behind it (latest-only coalescing), so a slow first
  // POST can never complete after a newer observation and leave the server
  // holding stale state - the chain sends at most the in-flight report
  // plus the newest pending one, in order. Never blocks its caller: the
  // chain runs in the background, punkFetch never rejects, a 404 (old
  // server) collapses to null, and no delivery or ACK ever waits on a
  // report. A dead/unbound session's chain drains and stops. opts:
  // {lastError, lastAttempt, nextAttemptMs}. Timestamps are RFC3339
  // quantized to the second, so a re-computed retry time inside the same
  // second does not dodge the dedup window.
  function punkDiagReport(st, state, opts) {
    try {
      if (!messagingEnabled || !st || !st.registered) return
      const ns = punkDiagNamespace()
      if (!ns) return
      const o = opts || {}
      const now = Date.now()
      const fields = {
        agent: st.agent,
        client: "opencode",
        delivery_mode: "idle_wake",
        state: state,
      }
      fields.pending_ack_count = Math.max(0, Math.min(st.delivered ? st.delivered.size : 0, 999))
      fields.wake_count = Math.max(0, Math.min(punkDiagWindowWakeCount(st), 999))
      if (o.lastError) fields.last_error = String(o.lastError).slice(0, 64)
      if (o.lastAttempt) fields.last_attempt_at = new Date(Math.round(now / 1000) * 1000).toISOString()
      if (typeof o.nextAttemptMs === "number" && o.nextAttemptMs > 0) {
        fields.next_attempt_at = new Date(Math.round((now + o.nextAttemptMs) / 1000) * 1000).toISOString()
      }
      const sig =
        fields.state +
        "|" +
        fields.delivery_mode +
        "|" +
        (fields.last_error || "") +
        "|" +
        fields.pending_ack_count +
        "|" +
        fields.wake_count +
        "|" +
        (fields.next_attempt_at || "") +
        "|" +
        (fields.last_attempt_at || "")
      if (st.punkDiagSig === sig && now - (st.punkDiagAt || 0) < PUNK_DIAG_DEDUP_MS) return
      st.punkDiagSig = sig
      st.punkDiagAt = now
      st.punkDiagPending = fields
      if (st.punkDiagSending) return
      st.punkDiagSending = true
      const run = async () => {
        try {
          while (st.punkDiagPending) {
            if (!messagingEnabled || !punkInboxStAlive(st)) {
              st.punkDiagPending = null
              return
            }
            const next = st.punkDiagPending
            st.punkDiagPending = null
            await punkFetch("/v1/namespaces/" + encodeURIComponent(ns) + "/messages/diagnostics", {
              method: "POST",
              body: JSON.stringify(next),
            })
          }
        } catch (err) {
          st.punkDiagPending = null
        } finally {
          st.punkDiagSending = false
        }
      }
      run()
    } catch (err) {
      // A diagnostic must never break its caller.
    }
  }

  // punkDiagTransition reports a busy/idle transition for whichever session
  // state currently exists. Unbound or still-registering sessions report
  // nothing: there is no confirmed member to attach an observation to yet,
  // and inventing one would be dishonest.
  function punkDiagTransition(sessionID, state) {
    if (!messagingEnabled || !sessionID) return
    const st = punkSessions.get(sessionID)
    if (st) punkDiagReport(st, state)
  }
`
