#!/bin/sh
# Durable crash capture + auto-restart supervisor for the Plurality server.
#
# Runs /app/Plurality as a child. Whenever the server process exits for any
# reason it records the exact exit code / signal plus recent server output to
# /app/data/crash.log (persistent volume), then immediately restarts it. This
# (a) makes every death observable even when nothing else captures stderr and
# (b) keeps the UI available after a crash instead of waiting for an external
# restart.
#
# PID 1 is still tini (see dockerfile ENTRYPOINT) so zombies are reaped and
# signals (SIGTERM on `docker stop`) are forwarded to this script, which
# forwards them to the server and exits cleanly.
set -u

# Enable core dumps + full Go crash traces so a native crash (C segv in
# sqlite/vec/http) leaves evidence on the persistent volume instead of dying
# invisibly. ulimit -c unlimited allows core files; GOTRACEBACK=crash makes
# the Go runtime print ALL goroutine stacks before aborting on a fatal/native
# error. Coredumps land in /app/data/cores (persistent).
mkdir -p /app/data/cores
ulimit -c unlimited 2>/dev/null || true
export GOTRACEBACK=crash

LOG=/app/data/crash.log
OUT=/app/data/server.log
mkdir -p /app/data
echo "=== supervisor.sh $(date -u +%FT%T) pid $$ (trap TERM/INT) ===" >> "$LOG"

# --- CPU watchdog -----------------------------------------------------------
# A runaway LLM/tool loop can pin the server at hundreds of % CPU (observed:
# a Plurality instance stuck at ~415% for 24h+ after re-creating its
# container). The in-process loop guards eventually halt the loop, but fast
# providers + repeated cron/webhook triggers can burn whole cores for minutes
# before they trip, and without shell access the goroutine stacks that would
# name the culprit are never captured. This watchdog samples the child's
# utime+stime from /proc/<pid>/stat (near-zero cost) and, when CPU stays
# above CPU_WATCHDOG_THRESHOLD for CPU_WATCHDOG_SAMPLES consecutive samples,
# sends SIGQUIT: the Go runtime dumps ALL goroutine stacks to the server log
# (persistent volume, via GOTRACEBACK=crash) and terminates, so the crash
# log + restart loop below captures the evidence and the startup-time
# RecoverStuckConversations pass idles whatever loop was spinning.
#
# Disabled by default (CPU_WATCHDOG_THRESHOLD=0). Set e.g. 300 to trip at
# ~3 cores sustained for CPU_WATCHDOG_SAMPLES*CPU_WATCHDOG_INTERVAL seconds
# (default 36 * 10s = 6 minutes).
CPU_WATCHDOG_THRESHOLD="${CPU_WATCHDOG_THRESHOLD:-0}"
CPU_WATCHDOG_SAMPLES="${CPU_WATCHDOG_SAMPLES:-36}"
CPU_WATCHDOG_INTERVAL="${CPU_WATCHDOG_INTERVAL:-10}"
# ---------------------------------------------------------------------------

forward() {
  echo "=== supervisor: received signal $(date -u +%FT%T); stopping server ===" >> "$LOG"
  kill -TERM "$SPID" 2>/dev/null
  exit 0
}
trap forward TERM INT

# PR #30 made /app/Plurality a SHIM that execs this supervisor, so running
# /app/Plurality here would recurse infinitely (startup loop). Run the real
# binary; fall back to the old direct path on pre-#30 images.
BIN=/app/Plurality.bin
[ -x "$BIN" ] || BIN=/app/Plurality
restart_delay=0
while true; do
  echo "=== $(date -u +%FT%T) starting $BIN (attempt after ${restart_delay}s) ===" >> "$LOG"
  "$BIN" >> "$OUT" 2>&1 &
  SPID=$!

  # Arm the CPU watchdog for this server instance. The subshell exits on its
  # own once the child dies (kill -0 fails) or after it fires once; the
  # /proc/<pid>/stat starttime check guards against PID reuse across
  # restarts so a stale watcher can never signal the wrong process.
  if [ "${CPU_WATCHDOG_THRESHOLD:-0}" -gt 0 ] 2>/dev/null; then
    (
      start_ticks="$(awk '{print $22}' "/proc/$SPID/stat" 2>/dev/null || true)"
      samples=0
      prev_ticks=""
      while kill -0 "$SPID" 2>/dev/null; do
        cur_start="$(awk '{print $22}' "/proc/$SPID/stat" 2>/dev/null || true)"
        [ -n "$cur_start" ] && [ "$cur_start" != "$start_ticks" ] && exit 0
        stat_now="$(awk '{print $14+$15}' "/proc/$SPID/stat" 2>/dev/null || true)"
        [ -z "$stat_now" ] && break
        if [ -n "$prev_ticks" ]; then
          # utime+stime are clock ticks (usually 100 Hz). Over the sample
          # interval, 100% of one core == interval*100 ticks, so the CPU
          # percentage equals delta_ticks / interval_seconds.
          delta=$((stat_now - prev_ticks))
          pct=$((delta / CPU_WATCHDOG_INTERVAL))
          if [ "$pct" -gt "$CPU_WATCHDOG_THRESHOLD" ]; then
            samples=$((samples + 1))
          else
            samples=0
          fi
          if [ "$samples" -ge "$CPU_WATCHDOG_SAMPLES" ]; then
            {
              echo "== CPU watchdog: server at ~${pct}% CPU for ${samples} samples — SIGQUIT for goroutine dump + restart $(date -u +%FT%T) =="
            } >> "$LOG"
            kill -QUIT "$SPID" 2>/dev/null
            exit 0
          fi
        fi
        prev_ticks=$stat_now
        sleep "$CPU_WATCHDOG_INTERVAL"
      done
    ) &
  fi

  wait "$SPID"
  rc=$?
  {
    echo "== server exited rc=$rc at $(date -u +%FT%T) =="
    echo "--- uptime $(awk '{printf "%.1fs", $1}' /proc/uptime) ---"
    echo "--- server log tail ---"
    tail -n 40 "$OUT" 2>/dev/null | tail -n 40
    echo "--- end ---"
  } >> "$LOG"
  # brief pause so a hard crash loop doesn't spin the CPU; then restart
  sleep 2
done