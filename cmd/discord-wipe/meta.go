package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/erfianugrah/discord-wipe-go/internal/export"
	"github.com/erfianugrah/discord-wipe-go/internal/snowflake"
)

// ---------------------------------------------------------------------------
// verify
// ---------------------------------------------------------------------------

var cmdVerify = &cobra.Command{
	Use:   "verify",
	Short: "Check that the token works.",
	Run: func(cmd *cobra.Command, args []string) {
		c := newClient()
		me, err := c.GetMe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL — %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("OK — @%s (id=%s)\n", me.Username, me.ID)
	},
}

// ---------------------------------------------------------------------------
// discover
// ---------------------------------------------------------------------------

var cmdDiscover = &cobra.Command{
	Use:   "discover",
	Short: "Show live guilds, DMs, and export contents.",
	Run: func(cmd *cobra.Command, args []string) {
		c := newClient()
		me, err := c.GetMe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL — %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("You: @%s (id=%s)\n\n", me.Username, me.ID)

		fmt.Println("== Live guilds ==")
		guilds, err := c.ListGuilds()
		if err != nil {
			fmt.Fprintf(os.Stderr, "list guilds: %v\n", err)
		}
		for _, g := range guilds {
			fmt.Printf("  %-20s %s\n", g.ID, g.Name)
		}
		fmt.Println()

		fmt.Println("== Open DM channels ==")
		dms, err := c.ListDMs()
		if err != nil {
			fmt.Fprintf(os.Stderr, "list DMs: %v\n", err)
		}
		for _, ch := range dms {
			kind := "DM"
			if ch.Type == 3 {
				kind = "GROUP_DM"
			}
			var names []string
			for _, r := range ch.Recipients {
				names = append(names, r.Username)
			}
			fmt.Printf("  %-20s %-8s [%v]\n", ch.ID, kind, join(names, ","))
		}
		fmt.Println()

		if _, err := os.Stat(exportDir); err == nil {
			fmt.Printf("== Export at %s ==\n", exportDir)
			chans, err := export.ReadExport(exportDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "read export: %v\n", err)
				return
			}
			total := 0
			for _, ch := range chans {
				fmt.Printf("  %-20s %-12s %6d  %s\n", ch.ID, ch.Type, ch.MsgCount, trunc(ch.Name, 60))
				total += ch.MsgCount
			}
			fmt.Printf("-- %d channels, %d messages --\n", len(chans), total)
		} else {
			fmt.Printf("(no export at %s)\n", exportDir)
		}
	},
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

var cmdStatus = &cobra.Command{
	Use:   "status",
	Short: "Read state.json and print a summary (no API calls).",
	Run: func(cmd *cobra.Command, args []string) {
		s := newState()
		fi, err := os.Stat(statePath)
		sizeKB := float64(0)
		if err == nil {
			sizeKB = float64(fi.Size()) / 1024
		}
		fmt.Printf("state file:       %s (%.1f KB)\n", statePath, sizeKB)
		fmt.Printf("deleted IDs:      %d\n", s.Len())
		fmt.Printf("export consumed:  %v\n", s.ExportConsumed)
		fmt.Printf("last pass:        %s (%s)\n", nonempty(s.LastPassAt, "never"), ageStr(s.LastPassAt))
		fmt.Printf("last start:       %s (%s)\n", nonempty(s.LastStartedAt, "never"), ageStr(s.LastStartedAt))
		fmt.Printf("restart_burst:    %d\n", s.RestartBurst)

		hb := filepath.Join(filepath.Dir(statePath), "heartbeat")
		if hbFi, err := os.Stat(hb); err == nil {
			fmt.Printf("heartbeat:        %s (%.0fs ago)\n", hb, time.Since(hbFi.ModTime()).Seconds())
		} else {
			fmt.Println("heartbeat:        (none)")
		}

		corrupts, _ := filepath.Glob(filepath.Join(filepath.Dir(statePath), "state.json.corrupt-*"))
		if len(corrupts) > 0 {
			fmt.Printf("corrupt backups:  %d (%s most recent)\n", len(corrupts), filepath.Base(corrupts[len(corrupts)-1]))
		}

		if s.Len() > 0 {
			ids := sortedKeysSlice(s.Deleted, 3)
			fmt.Printf("sample IDs:       %v\n", ids)
		}
	},
}

// ---------------------------------------------------------------------------
// seed-from-export
// ---------------------------------------------------------------------------

var cmdSeed = &cobra.Command{
	Use:   "seed-from-export",
	Short: "Mark export messages older than cutoff as already-deleted (recovery).",
	Run: func(cmd *cobra.Command, args []string) {
		s := newState()
		cutoff := time.Now().UTC().Add(-time.Duration(retentionDays*24) * time.Hour)
		before := s.Len()
		fmt.Printf("[seed] state=%s: %d IDs already marked, export_consumed=%v\n",
			statePath, before, s.ExportConsumed)
		fmt.Printf("[seed] cutoff=%s (retention=%.0fd)\n", cutoff.Format(time.RFC3339), retentionDays)

		chans, err := export.ReadExport(exportDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[seed] %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[seed] %d channels in export\n", len(chans))

		seeded := 0
		skippedRecent := 0
		for _, ch := range chans {
			msgsPath := filepath.Join(exportDir, "c"+ch.ID, "messages.json")
			msgs, err := export.ReadChannelMessages(msgsPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[seed] %s: skip (%v)\n", ch.ID, err)
				continue
			}
			for _, m := range msgs {
				if m.ID == "" {
					continue
				}
				if s.IsDeleted(m.ID) {
					continue
				}
				if m.Timestamp != "" {
					ts, err := export.ParseTimestamp(m.Timestamp)
					if err == nil && !ts.Before(cutoff) {
						skippedRecent++
						continue
					}
				}
				s.Mark(m.ID)
				seeded++
			}
		}

		s.ExportConsumed = true
		s.Save() //nolint:errcheck
		fmt.Printf("[seed] seeded %d new IDs (was %d, now %d); left %d recent unmarked; export_consumed=true\n",
			seeded, before, s.Len(), skippedRecent)
		fmt.Println("[seed] done.")
	},
}

func init() {
	cmdDiscover.Flags().StringVar(&exportDir, "export-dir", "/data/export/Messages", "export directory")
	cmdStatus.Flags().StringVar(&statePath, "state", "/data/state/state.json", "state file path")
	cmdSeed.Flags().StringVar(&statePath, "state", "/data/state/state.json", "state file path")
	cmdSeed.Flags().StringVar(&exportDir, "export-dir", "/data/export/Messages", "export directory")
	cmdSeed.Flags().Float64Var(&retentionDays, "retention-days", 14, "messages older than this are seeded")
}

// helpers

func nonempty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func ageStr(iso string) string {
	if iso == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds ago", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh ago", int(d.Hours()/24), int(d.Hours())%24)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func join(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	out := ss[0]
	for _, s := range ss[1:] {
		out += sep + s
	}
	return out
}

func sortedKeysSlice(m map[string]bool, n int) []string {
	var ids []string
	for k := range m {
		ids = append(ids, k)
	}
	// sort not needed for sampling
	if len(ids) > n {
		ids = ids[:n]
	}
	return ids
}

func mustSnowflake(s string) int64 {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return snowflake.At(time.Now().UTC())
		}
	}
	return snowflake.At(t)
}
