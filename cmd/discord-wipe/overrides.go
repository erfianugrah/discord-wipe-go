package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erfianugrah/discord-wipe-go/internal/snowflake"
)

// Per-scope retention overrides (`run` only).
//
// An override pins a shorter (or longer) retention window to one guild or
// channel while the rest of the account follows the global RETENTION_DAYS.
// Syntax per entry:  guild:<id>:<days>  or  channel:<id>:<days>
// Flag: --retention-override (repeatable). Env: RETENTION_OVERRIDES,
// comma-separated.
//
// Scope: overrides apply to the live catchup phase ONLY. The one-time export
// phase keys off message timestamps, not guilds (export channels carry no
// guild ID), so it always uses the global retention - an override LONGER than
// the global retention will not protect export messages from the export pass.
// Shorter overrides (the normal case) are unaffected: everything the export
// phase deletes is older than the override cutoff anyway.

// parseRetentionOverrides parses "scope:id:days" entries into a map keyed
// "scope:id". Any malformed entry is a hard error: a silently dropped
// retention rule deletes (or keeps) the wrong messages.
func parseRetentionOverrides(entries []string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		parts := strings.Split(e, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid override %q: want scope:id:days (e.g. guild:123456789:2)", e)
		}
		scope, id, daysStr := parts[0], parts[1], parts[2]
		if scope != "guild" && scope != "channel" {
			return nil, fmt.Errorf("invalid override %q: scope must be \"guild\" or \"channel\"", e)
		}
		if id == "" {
			return nil, fmt.Errorf("invalid override %q: empty id", e)
		}
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			return nil, fmt.Errorf("invalid override %q: id must be a snowflake integer", e)
		}
		days, err := strconv.ParseFloat(daysStr, 64)
		if err != nil || days < 0 {
			return nil, fmt.Errorf("invalid override %q: days must be a number >= 0", e)
		}
		out[scope+":"+id] = days
	}
	return out, nil
}

// targetCutoffSF returns the delete-cutoff snowflake for one scope: the
// override value when scope:id matches, else the global retention. The
// returned days is the effective window; ok reports whether an override
// applied (for logging).
func targetCutoffSF(now time.Time, globalDays float64, scope, id string, overrides map[string]float64) (sf int64, days float64, ok bool) {
	days = globalDays
	if ov, hit := overrides[scope+":"+id]; hit {
		days = ov
		ok = true
	}
	return snowflake.At(retentionCutoff(now, days)), days, ok
}

// resolveSlice is resolveFloat for string-slice flags: an explicitly-passed
// flag wins; otherwise the value comes from the comma-separated env var.
// Only cmdRun binds retentionOverrides, so there is no cross-command clobber
// to guard against - this exists for the env path.
func resolveSlice(cmd *cobra.Command, name, envKey string) []string {
	if cmd.Flags().Changed(name) {
		v, _ := cmd.Flags().GetStringSlice(name)
		return v
	}
	raw := os.Getenv(envKey)
	if raw == "" {
		return nil
	}
	var out []string
	for _, e := range strings.Split(raw, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}
