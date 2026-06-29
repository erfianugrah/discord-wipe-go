package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/erfianugrah/discord-wipe-go/internal/discord"
	"github.com/erfianugrah/discord-wipe-go/internal/export"
	"github.com/erfianugrah/discord-wipe-go/internal/snowflake"
	"github.com/erfianugrah/discord-wipe-go/internal/state"
)

const (
	restartBurstMax    = 5
	restartBurstWindow = 600 // seconds
)

var cmdRun = &cobra.Command{
	Use:   "run",
	Short: "Run the rolling-retention wipe loop.",
	Long: `Run one or more wipe passes. Each pass:
  1. Export phase (first pass only): deletes messages from a Discord
     data export that are older than the retention cutoff.
  2. Live catchup: sweeps every guild + DM via the search API.

With --watch, loops forever. Otherwise runs one pass and exits.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := signalCtx()
		defer cancel()

		slog.Info("discord-wipe", "version", version)

		// Re-resolve env-backed flags. Several commands bind the SAME package
		// globals with DIFFERENT defaults (notably purge binds retentionDays=0).
		// Go runs init() in filename order, so search.go (purge) registers AFTER
		// run.go and clobbers the shared variable's starting value; pflag does
		// not reset an unset flag during Parse. Without this guard `run`
		// inherited purge's retentionDays=0 and deleted EVERY message regardless
		// of age (the "deletes everything" bug). Honour an explicitly-passed
		// flag; otherwise re-derive from the environment / this command's default.
		retentionDays = resolveFloat(cmd, "retention-days", "RETENTION_DAYS", 14)
		deleteDelay = resolveFloat(cmd, "delete-delay", "DELETE_DELAY", 1.0)
		searchDelay = resolveFloat(cmd, "search-delay", "SEARCH_DELAY", 15.0)
		intervalHours = resolveFloat(cmd, "interval-hours", "INTERVAL_HOURS", 24)
		watch = resolveBool(cmd, "watch", "WATCH", false)
		statePath = resolveString(cmd, "state", "STATE_PATH", "/data/state/state.json")
		exportDir = resolveString(cmd, "export-dir", "EXPORT_DIR", "/data/export/Messages")

		// --- state init with parking ---
		st, err := state.New(statePath)
		if err != nil {
			park("state-unwritable", "STATE FILE UNWRITABLE",
				fmt.Sprintf("reason: %v", err),
				"Common causes:\n"+
					"  - The bind-mount target doesn't exist on the host.\n"+
					"  - The host disk hosting the state dir is full.\n"+
					"  - The filesystem has frozen (Unraid shfs / array degraded).\n"+
					"  - Permissions are wrong (state dir should be 99:100 on Unraid).\n",
			)
		}

		// --- restart burst guard ---
		now := time.Now().UTC()
		if st.LastStartedAt != "" {
			prev, _ := time.Parse(time.RFC3339, st.LastStartedAt)
			if now.Sub(prev).Seconds() < restartBurstWindow {
				st.RestartBurst++
			} else {
				st.RestartBurst = 1
			}
		} else {
			st.RestartBurst = 1
		}
		st.LastStartedAt = now.Format(time.RFC3339)
		if st.RestartBurst > restartBurstMax {
			park("restart-burst", "RESTART BURST DETECTED",
				fmt.Sprintf("This container has started %d times within the last %ds.",
					st.RestartBurst, restartBurstWindow),
				"Common causes:\n"+
					"  - A broken release on :main.\n"+
					"  - Token missing / file empty.\n"+
					"  - Required path doesn't exist.\n",
			)
		}
		st.Save() //nolint:errcheck

		// --- metrics server ---
		if metricsEnabled {
			startMetrics(st)
		}

		// --- auth ---
		c := newClient()
		me, err := c.GetMe()
		if err != nil {
			if ae, ok := err.(*discord.AuthError); ok {
				park("token-rejected", "DISCORD TOKEN REJECTED",
					fmt.Sprintf("reason: %s", ae.Message),
					"Discord user tokens have NO refresh flow.\n"+
						"  - You logged out / logged back in.\n"+
						"  - You changed your password.\n"+
						"  - Discord rotated it.\n",
				)
			}
			fmt.Fprintf(os.Stderr, "[FATAL] auth error: %v\n", err)
			os.Exit(2)
		}
		slog.Info("authenticated", "user", me.Username, "id", me.ID)

		// Clear burst counter on successful auth
		if st.RestartBurst > 0 {
			st.RestartBurst = 0
			st.Save() //nolint:errcheck
		}

		slog.Info("state", "path", statePath, "deleted", st.Len(),
			"export_consumed", st.ExportConsumed, "restart_burst", st.RestartBurst)

		// --- main loop ---
		for {
			select {
			case <-ctx.Done():
				slog.Info("stop signal — exiting")
				return
			default:
			}

			// Preflight: re-verify identity
			fresh, err := c.GetMe()
			if err != nil {
				if ae, ok := err.(*discord.AuthError); ok {
					park("token-rejected", "DISCORD TOKEN REJECTED MID-LOOP",
						fmt.Sprintf("reason: %s", ae.Message),
						"Token expired or was rotated during the run.\n",
					)
				}
				slog.Error("preflight auth error", "err", err)
				time.Sleep(60 * time.Second)
				continue
			}
			if fresh.ID != me.ID {
				park("identity-changed", "IDENTITY CHANGED",
					fmt.Sprintf("was @%s (id=%s), now @%s (id=%s)",
						me.Username, me.ID, fresh.Username, fresh.ID),
					"Token now belongs to a different account. Rotate the token.\n",
				)
			}

			cutoff := retentionCutoff(time.Now().UTC(), retentionDays)
			slog.Info("pass start", "cutoff", cutoff.Format(time.RFC3339), "retention_days", retentionDays)
			t0 := time.Now()

			// --- export phase ---
			if _, err := os.Stat(exportDir); err == nil && !st.ExportConsumed {
				phaseExport(ctx, c, me.ID, st, cutoff, dryRun)
			} else if os.IsNotExist(err) {
				slog.Info("no export dir, skipping export phase", "dir", exportDir)
			}

			// --- live catchup phase ---
			if ctx.Err() == nil {
				phaseLiveCatchupAll(ctx, c, me.ID, st, cutoff, dryRun)
			}

			st.LastPassAt = time.Now().UTC().Format(time.RFC3339)
			st.Save() //nolint:errcheck

			elapsed := time.Since(t0)
			slog.Info("pass complete", "elapsed", durStr(elapsed), "seconds", elapsed.Seconds())

			if !watch {
				return
			}

			sleepFor := time.Duration(intervalHours * float64(time.Hour))
			slog.Info("sleeping", "duration", sleepFor, "next", time.Now().Add(sleepFor).Format(time.RFC3339))
			// Sleep in 60s chunks, touching heartbeat each minute so
			// docker HEALTHCHECK stays green across long inter-pass waits.
			slept := time.Duration(0)
			for slept < sleepFor {
				chunk := 60 * time.Second
				if sleepFor-slept < chunk {
					chunk = sleepFor - slept
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(chunk):
				}
				slept += chunk
				st.Save() //nolint:errcheck // touches heartbeat
			}
		}
	},
}

// ---------------------------------------------------------------------------
// Export phase
// ---------------------------------------------------------------------------

func phaseExport(ctx context.Context, c *discord.Client, meID string, st *state.State,
	cutoff time.Time, dryRun bool) {

	slog.Info("export phase: reading", "dir", exportDir)
	channels, err := export.ReadExport(exportDir)
	if err != nil {
		slog.Error("read export", "err", err)
		return
	}
	slog.Info("export phase", "channels", len(channels), "cutoff", cutoff.Format(time.RFC3339))

	type exportCounts struct {
		ok, gone, forbidden, skipRecent, skipDone int
	}
	var ec exportCounts

	deleteD := time.Duration(deleteDelay * float64(time.Second))

	// Pre-parse all channels
	type channelData struct {
		ch   export.Channel
		msgs []export.Message
	}
	var parsed []channelData
	grandTotal := 0
	alreadyDoneTotal := 0
	for _, ch := range channels {
		msgsPath := filepath.Join(exportDir, "c"+ch.ID, "messages.json")
		msgs, err := export.ReadChannelMessages(msgsPath)
		if err != nil {
			slog.Warn("skip corrupt messages.json", "channel", ch.ID, "err", err)
			continue
		}
		chDone := 0
		for _, m := range msgs {
			if st.IsDeleted(m.ID) {
				chDone++
			}
		}
		grandTotal += len(msgs)
		alreadyDoneTotal += chDone
		parsed = append(parsed, channelData{ch, msgs})
	}

	resumePct := 0.0
	if grandTotal > 0 {
		resumePct = 100.0 * float64(alreadyDoneTotal) / float64(grandTotal)
	}
	slog.Info("export resume",
		"already_done", alreadyDoneTotal, "total", grandTotal,
		"pct", fmt.Sprintf("%.1f%%", resumePct),
		"remaining", grandTotal-alreadyDoneTotal)

	for ci, pd := range parsed {
		select {
		case <-ctx.Done():
			return
		default:
		}

		prefix := fmt.Sprintf("[export %d/%d] %-10s %-50s", ci+1, len(parsed), pd.ch.Type, trunc(pd.ch.Name, 50))

		// Build target list
		var targets []export.Message
		chSkipDone := 0
		for _, m := range pd.msgs {
			if st.IsDeleted(m.ID) {
				ec.skipDone++
				chSkipDone++
				continue
			}
			if m.Timestamp != "" {
				ts, err := export.ParseTimestamp(m.Timestamp)
				if err == nil && !ts.Before(cutoff) {
					ec.skipRecent++
					continue
				}
			}
			targets = append(targets, m)
		}

		if len(targets) == 0 {
			if chSkipDone > 0 {
				slog.Info(prefix, "already_done", fmt.Sprintf("%d/%d", chSkipDone, len(pd.msgs)))
			}
			continue
		}

		slog.Info(prefix, "to_delete", len(targets))

		extraSleep := 0.0
		floor429 := 0.0
		for _, m := range targets {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if dryRun {
				ec.ok++
				st.Mark(m.ID)
				continue
			}

			for {
				result := c.DeleteMessage(pd.ch.ID, m.ID)
				switch result.Status {
				case "retry":
					slog.Debug("rate-limited", "sleep", fmt.Sprintf("%.1fs", result.RetryAfter))
					extraSleep = result.RetryAfter
					floor429 = result.RetryAfter
					time.Sleep(time.Duration(result.RetryAfter * float64(time.Second)))
					continue
				case "ok":
					ec.ok++
					if result.RetryAfter > 0 {
						extraSleep = result.RetryAfter
						if extraSleep < floor429 {
							extraSleep = floor429
						}
					}
				case "gone":
					ec.gone++
					if result.RetryAfter > 0 {
						extraSleep = result.RetryAfter
						if extraSleep < floor429 {
							extraSleep = floor429
						}
					}
				case "forbidden":
					ec.forbidden++
				}
				break
			}
			st.Mark(m.ID)
			sleepFor := deleteD.Seconds()
			if extraSleep > sleepFor {
				sleepFor = extraSleep
			}
			time.Sleep(time.Duration(sleepFor * float64(time.Second)))
		}
		st.Save() //nolint:errcheck
	}

	if !dryRun {
		st.ExportConsumed = true
	}
	st.Save() //nolint:errcheck
	slog.Info("export done", "ok", ec.ok, "gone", ec.gone, "forbidden", ec.forbidden,
		"skip_recent", ec.skipRecent, "skip_done", ec.skipDone)
}

// ---------------------------------------------------------------------------
// Live catchup (enumerate all guilds + DMs)
// ---------------------------------------------------------------------------

func phaseLiveCatchupAll(ctx context.Context, c *discord.Client, meID string, st *state.State,
	cutoff time.Time, dryRun bool) {

	cutoffSF := snowflake.At(cutoff)
	guilds, _ := c.ListGuilds()
	dms, _ := c.ListDMs()
	slog.Info("catchup", "guilds", len(guilds), "dms", len(dms), "cutoff", cutoff.Format(time.RFC3339))

	var targets []ScopedTarget
	for _, g := range guilds {
		skip := false
		for _, eg := range excludeGuilds {
			if eg == g.ID {
				skip = true
				break
			}
		}
		if skip {
			slog.Info("skip excluded guild", "id", g.ID, "name", g.Name)
			continue
		}
		targets = append(targets, ScopedTarget{"guild", g.ID, g.Name})
	}
	for _, ch := range dms {
		skip := false
		for _, ec := range excludeChans {
			if ec == ch.ID {
				skip = true
				break
			}
		}
		if skip {
			slog.Info("skip excluded channel", "id", ch.ID)
			continue
		}
		kind := "dm"
		if ch.Type == 3 {
			kind = "groupdm"
		}
		targets = append(targets, ScopedTarget{"channel", ch.ID, kind + ":" + ch.ID})
	}

	counts := liveCatchup(ctx, c, meID, st, targets, cutoffSF, dryRun)
	slog.Info("catchup done", "ok", counts.ok, "gone", counts.gone, "forbidden", counts.forbidden)
}

// ---------------------------------------------------------------------------
// Parking
// ---------------------------------------------------------------------------

func park(reason, title, detail, help string) {
	banner := fmt.Sprintf("\n%s\n%s\n[FATAL] %s\n\n%s\n\n%s\nSleeping until SIGTERM.\n",
		strings.Repeat("=", 72), strings.Repeat("=", 72), title, detail, help)
	fmt.Fprint(os.Stderr, banner)
	notify(title, detail)
	select {}
}

func notify(title, body string) {
	if ntfyURL == "" {
		return
	}
	msg := body
	if len(msg) > 3000 {
		msg = msg[:3000]
	}
	req, _ := http.NewRequest("POST", ntfyURL, strings.NewReader(msg))
	if req != nil {
		req.Header.Set("Title", "discord-wipe: "+title)
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "warning,robot")
		http.DefaultClient.Do(req) //nolint:errcheck
	}
}

// ---------------------------------------------------------------------------
// Minimal Prometheus metrics
// ---------------------------------------------------------------------------

var (
	metricDeletesOK        atomic.Int64
	metricDeletesGone      atomic.Int64
	metricDeletesForbidden atomic.Int64
	metricState            *state.State // set by startMetrics
)

func startMetrics(st *state.State) {
	metricState = st
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP discord_wipe_deletes_total Delete operations by outcome\n")
		fmt.Fprintf(w, "# TYPE discord_wipe_deletes_total counter\n")
		fmt.Fprintf(w, "discord_wipe_deletes_total{outcome=\"ok\"} %d\n", metricDeletesOK.Load())
		fmt.Fprintf(w, "discord_wipe_deletes_total{outcome=\"gone\"} %d\n", metricDeletesGone.Load())
		fmt.Fprintf(w, "discord_wipe_deletes_total{outcome=\"forbidden\"} %d\n", metricDeletesForbidden.Load())
		fmt.Fprintf(w, "# HELP discord_wipe_state_deleted_count IDs tracked in state.deleted\n")
		fmt.Fprintf(w, "# TYPE discord_wipe_state_deleted_count gauge\n")
		fmt.Fprintf(w, "discord_wipe_state_deleted_count %d\n", metricState.Len())
		fmt.Fprintf(w, "# HELP discord_wipe_export_consumed 1 if export phase has run\n")
		fmt.Fprintf(w, "# TYPE discord_wipe_export_consumed gauge\n")
		consumed := 0
		if metricState.ExportConsumed {
			consumed = 1
		}
		fmt.Fprintf(w, "discord_wipe_export_consumed %d\n", consumed)
	})
	go func() {
		slog.Info("metrics", "bind", metricsBind)
		if err := http.ListenAndServe(metricsBind, nil); err != nil {
			slog.Error("metrics server", "err", err)
		}
	}()
}

// ---------------------------------------------------------------------------
// CLI flags for run
// ---------------------------------------------------------------------------

func init() {
	cmdRun.Flags().StringVar(&statePath, "state", envDefault("STATE_PATH", "/data/state/state.json"), "state file path")
	cmdRun.Flags().StringVar(&exportDir, "export-dir", envDefault("EXPORT_DIR", "/data/export/Messages"), "export directory")
	cmdRun.Flags().Float64Var(&retentionDays, "retention-days", envFloat("RETENTION_DAYS", 14), "messages older than N days are deleted")
	cmdRun.Flags().Float64Var(&deleteDelay, "delete-delay", envFloat("DELETE_DELAY", 1.0), "seconds between DELETE calls")
	cmdRun.Flags().Float64Var(&searchDelay, "search-delay", envFloat("SEARCH_DELAY", 15.0), "seconds between search page fetches")
	cmdRun.Flags().Float64Var(&intervalHours, "interval-hours", envFloat("INTERVAL_HOURS", 24), "hours between passes when --watch")
	cmdRun.Flags().BoolVar(&watch, "watch", envBool("WATCH", false), "loop forever instead of single pass")
	cmdRun.Flags().BoolVar(&dryRun, "dry-run", false, "report without deleting")
	cmdRun.Flags().StringSliceVar(&excludeGuilds, "exclude-guild", nil, "guild ID to skip (repeatable)")
	cmdRun.Flags().StringSliceVar(&excludeChans, "exclude-channel", nil, "channel ID to skip (repeatable)")

	// Env-driven flags
	cmdRun.Flags().StringVar(&ntfyURL, "ntfy-url", os.Getenv("NTFY_URL"), "ntfy.sh webhook URL")
	cmdRun.Flags().StringVar(&metricsBind, "metrics-bind", envDefault("METRICS_BIND", "0.0.0.0:9090"), "metrics bind address")
	cmdRun.Flags().BoolVar(&metricsEnabled, "metrics-enabled", envBool("METRICS_ENABLED", true), "enable /metrics")
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var result float64
	if _, err := fmt.Sscanf(v, "%f", &result); err != nil {
		return fallback
	}
	return result
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "yes"
}

// retentionCutoff returns the timestamp `days` days before `now`. Messages with
// a timestamp strictly older than the cutoff are eligible for deletion; days=0
// yields `now` (delete everything). Centralised so the float→Duration
// conversion lives in one tested place.
func retentionCutoff(now time.Time, days float64) time.Time {
	return now.Add(-time.Duration(days*24) * time.Hour)
}

// resolveFloat returns the effective value of a float flag that is bound to a
// shared package global. If the flag was explicitly set on the command line we
// honour the parsed value; otherwise we re-derive from the environment (or the
// supplied default) so a default registered by a DIFFERENT command can't leak
// in via the shared variable. See the guard block in cmdRun.Run for why.
func resolveFloat(cmd *cobra.Command, name, envKey string, def float64) float64 {
	if cmd.Flags().Changed(name) {
		v, _ := cmd.Flags().GetFloat64(name)
		return v
	}
	if envKey != "" {
		return envFloat(envKey, def)
	}
	return def
}

// resolveBool is resolveFloat for bool flags.
func resolveBool(cmd *cobra.Command, name, envKey string, def bool) bool {
	if cmd.Flags().Changed(name) {
		v, _ := cmd.Flags().GetBool(name)
		return v
	}
	if envKey != "" {
		return envBool(envKey, def)
	}
	return def
}

// resolveString is resolveFloat for string flags.
func resolveString(cmd *cobra.Command, name, envKey, def string) string {
	if cmd.Flags().Changed(name) {
		v, _ := cmd.Flags().GetString(name)
		return v
	}
	if envKey != "" {
		return envDefault(envKey, def)
	}
	return def
}
