package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erfianugrah/discord-wipe-go/internal/discord"
	"github.com/erfianugrah/discord-wipe-go/internal/snowflake"
)

var (
	exportOutput    string
	exportAll       bool
	exportPerChan   int
	leaveInactive   int
	closeInactive   int
	leaveDryRun     bool
	leaveConfirm    bool
)

// ---------------------------------------------------------------------------
// export subcommand
// ---------------------------------------------------------------------------

var cmdExport = &cobra.Command{
	Use:   "export",
	Short: "Backup your messages as JSON before wiping.",
	Long: `Download your messages from guilds or channels and save them as
JSON files organized by server/channel. Use this as a safety net
before running 'purge'.

Examples:
  discord-wipe export --guild 123456789 --output ./backup/
  discord-wipe export --output ./backup/   # export everything
  discord-wipe export --channel 987654321 --output ./dm-backup/`,
	RunE: runExport,
}

func runExport(cmd *cobra.Command, args []string) error {
	ctx, cancel := signalCtx()
	defer cancel()

	c := newClient()
	me, err := c.GetMe()
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	fmt.Printf("Export: authenticated as @%s\n", me.Username)

	if exportOutput == "" {
		return fmt.Errorf("--output is required")
	}

	// Build scope list
	type scope struct {
		typ, id, label string
	}
	var scopes []scope

	switch {
	case len(searchChannels) > 0:
		dms, _ := c.ListDMs()
		dmMap := map[string]string{}
		for _, dm := range dms {
			var names []string
			for _, r := range dm.Recipients {
				names = append(names, r.Username)
			}
			dmMap[dm.ID] = "DM_" + sanitize(strings.Join(names, "_"))
		}
		for _, cid := range searchChannels {
			label := dmMap[cid]
			if label == "" {
				label = "channel_" + cid
			}
			scopes = append(scopes, scope{"channel", cid, label})
		}
	case len(searchGuilds) > 0:
		for _, gid := range searchGuilds {
			g, err := c.GetGuild(gid)
			gname := gid
			if err == nil {
				gname = sanitize(g.Name)
			}
			scopes = append(scopes, scope{"guild", gid, gname})
		}
	default:
		// Export everything
		guilds, _ := c.ListGuilds()
		dms, _ := c.ListDMs()
		for _, g := range guilds {
			scopes = append(scopes, scope{"guild", g.ID, sanitize(g.Name)})
		}
		for _, dm := range dms {
			var names []string
			for _, r := range dm.Recipients {
				names = append(names, r.Username)
			}
			label := "DM_" + sanitize(strings.Join(names, "_"))
			scopes = append(scopes, scope{"channel", dm.ID, label})
		}
	}

	if len(scopes) == 0 {
		return fmt.Errorf("no scopes to export")
	}

	// Snowflake bounds
	maxID := snowflake.At(time.Now().UTC())
	if beforeDate != "" {
		maxID = mustSnowflake(beforeDate)
	}
	minID := int64(0)
	if afterDate != "" {
		minID = mustSnowflake(afterDate)
	}

	baseDir := exportOutput
	totalMessages := 0
	totalChannels := 0

	for si, s := range scopes {
		select {
		case <-ctx.Done():
			fmt.Println("\nExport cancelled.")
			return nil
		default:
		}

		var channels []discord.Channel
		if s.typ == "guild" {
			chs, err := c.ListGuildChannels(s.id)
			if err != nil {
				fmt.Printf("[%d/%d] %s: skip (can't list channels: %v)\n", si+1, len(scopes), s.label, err)
				continue
			}
			// Only text channels (type 0)
			for _, ch := range chs {
				if ch.Type == 0 {
					channels = append(channels, ch)
				}
			}
		} else {
			channels = append(channels, discord.Channel{ID: s.id, Name: s.label, Type: 0})
		}

		for _, ch := range channels {
			select {
			case <-ctx.Done():
				fmt.Println("\nExport cancelled.")
				return nil
			default:
			}

			var allMsgs []discord.FetchedMessage
			before := maxID
			pageCount := 0

			for {
				if exportPerChan > 0 && len(allMsgs) >= exportPerChan {
					break
				}
				msgs, hasMore, err := c.FetchMessages(ch.ID, before, minID, 100)
				if err != nil {
					fmt.Printf("  [%s] fetch error: %v\n", ch.Name, err)
					break
				}
				if len(msgs) == 0 {
					break
				}
				// Filter to only our messages
				for _, m := range msgs {
					if m.Author.ID != me.ID {
						continue
					}
					allMsgs = append(allMsgs, m)
				}
				pageCount++
				if !hasMore {
					break
				}
				before = parseSnowflake(msgs[len(msgs)-1].ID)
				time.Sleep(500 * time.Millisecond)
			}

			if len(allMsgs) == 0 {
				continue
			}

			// Sort oldest-first
			sort.Slice(allMsgs, func(i, j int) bool {
				return parseSnowflake(allMsgs[i].ID) < parseSnowflake(allMsgs[j].ID)
			})

			// Write to disk
			var dir string
			if s.typ == "guild" {
				dir = filepath.Join(baseDir, "guilds", s.label)
			} else {
				dir = filepath.Join(baseDir, "dms")
			}
			os.MkdirAll(dir, 0o755)
			outPath := filepath.Join(dir, sanitize(ch.Name)+".json")

			data, err := json.MarshalIndent(allMsgs, "", "  ")
			if err != nil {
				fmt.Printf("  [%s] marshal error: %v\n", ch.Name, err)
				continue
			}
			if err := os.WriteFile(outPath, data, 0o644); err != nil {
				fmt.Printf("  [%s] write error: %v\n", ch.Name, err)
				continue
			}
			fmt.Printf("  [%s] %d messages → %s (%.1f KB)\n", ch.Name, len(allMsgs), outPath, float64(len(data))/1024)
			totalMessages += len(allMsgs)
			totalChannels++
		}
	}

	fmt.Printf("\n--- %d messages across %d channels exported to %s ---\n",
		totalMessages, totalChannels, baseDir)
	return nil
}

// ---------------------------------------------------------------------------
// leave subcommand
// ---------------------------------------------------------------------------

var cmdLeave = &cobra.Command{
	Use:   "leave",
	Short: "Leave servers you no longer need.",
	Long: `Leave one or more Discord servers. With --inactive N, only leaves
servers where your last message is older than N days (uses search API
to check activity before leaving).

Examples:
  discord-wipe leave --guild 123456789
  discord-wipe leave --inactive 180          # leave servers inactive > 180 days
  discord-wipe leave --inactive 180 --dry-run # preview without leaving`,
	RunE: runLeave,
}

func runLeave(cmd *cobra.Command, args []string) error {
	c := newClient()
	me, err := c.GetMe()
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	fmt.Printf("Leave: authenticated as @%s\n", me.Username)

	if len(searchGuilds) == 0 && leaveInactive == 0 {
		return fmt.Errorf("specify --guild GUILD_ID or --inactive DAYS")
	}

	var targets []discord.Guild
	if len(searchGuilds) > 0 {
		for _, gid := range searchGuilds {
			g, err := c.GetGuild(gid)
			if err != nil {
				fmt.Printf("  skip %s: %v\n", gid, err)
				continue
			}
			targets = append(targets, *g)
		}
	} else {
		guilds, err := c.ListGuilds()
		if err != nil {
			return fmt.Errorf("list guilds: %w", err)
		}
		cutoff := time.Now().UTC().Add(-time.Duration(leaveInactive*24) * time.Hour)
		cutoffSF := snowflake.At(cutoff)

		for _, g := range guilds {
			params := discord.SearchParams{
				AuthorID: me.ID,
				MaxID:    cutoffSF,
			}
			total, _, _ := c.SearchMessages("guild", g.ID, params)
			if total == 0 {
				targets = append(targets, g)
			} else if total < 0 {
				// No permission to search — we can still leave but warn
				fmt.Printf("  %s: can't check activity (no search permission)\n", g.Name)
			} else {
				fmt.Printf("  %s: %d messages in range — keeping\n", g.Name, total)
			}
		}
	}

	if len(targets) == 0 {
		fmt.Println("No servers to leave.")
		return nil
	}

	fmt.Printf("\n%d servers to leave:\n", len(targets))
	for _, g := range targets {
		fmt.Printf("  %s (%s)\n", g.Name, g.ID)
	}

	if leaveDryRun {
		fmt.Println("\n--dry-run: no servers left.")
		return nil
	}

	fmt.Print("\nType 'yes' to confirm: ")
	var confirm string
	fmt.Scanln(&confirm)
	if confirm != "yes" {
		fmt.Println("Cancelled.")
		return nil
	}

	left, failed := 0, 0
	for _, g := range targets {
		if err := c.LeaveGuild(g.ID); err != nil {
			fmt.Printf("  FAIL %s: %v\n", g.Name, err)
			failed++
		} else {
			fmt.Printf("  LEFT %s\n", g.Name)
			left++
		}
		time.Sleep(1 * time.Second)
	}

	fmt.Printf("\n%d left, %d failed\n", left, failed)
	return nil
}

// ---------------------------------------------------------------------------
// close-dms subcommand
// ---------------------------------------------------------------------------

var cmdCloseDMs = &cobra.Command{
	Use:   "close-dms",
	Short: "Close open DM channels.",
	Long: `Close your open DM channels. With --inactive N, only closes
DMs where your last message is older than N days.

Examples:
  discord-wipe close-dms
  discord-wipe close-dms --inactive 90
  discord-wipe close-dms --inactive 90 --dry-run`,
	RunE: runCloseDMs,
}

func runCloseDMs(cmd *cobra.Command, args []string) error {
	c := newClient()
	me, err := c.GetMe()
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	fmt.Printf("Close-DMs: authenticated as @%s\n", me.Username)

	dms, err := c.ListDMs()
	if err != nil {
		return fmt.Errorf("list DMs: %w", err)
	}

	type dmTarget struct {
		discord.DMChannel
		shouldClose bool
		reason      string
	}
	var targets []dmTarget

	if closeInactive > 0 {
		cutoff := time.Now().UTC().Add(-time.Duration(closeInactive*24) * time.Hour)
		cutoffSF := snowflake.At(cutoff)

		for _, dm := range dms {
			params := discord.SearchParams{
				AuthorID: me.ID,
				MaxID:    cutoffSF,
			}
			total, _, _ := c.SearchMessages("channel", dm.ID, params)
			if total == 0 {
				targets = append(targets, dmTarget{dm, true, fmt.Sprintf("inactive > %dd", closeInactive)})
			} else if total < 0 {
				targets = append(targets, dmTarget{dm, true, "can't check activity"})
			} else {
				// has recent messages — keep
			}
			time.Sleep(500 * time.Millisecond) // pace the search calls
		}
	} else {
		for _, dm := range dms {
			targets = append(targets, dmTarget{dm, true, "close all"})
		}
	}

	if len(targets) == 0 {
		fmt.Println("No DMs to close.")
		return nil
	}

	fmt.Printf("\n%d DMs to close:\n", len(targets))
	for _, t := range targets {
		var names []string
		for _, r := range t.Recipients {
			names = append(names, r.Username)
		}
		fmt.Printf("  DM:%s (%s) — %s\n", strings.Join(names, ","), t.ID, t.reason)
	}

	if dryRun {
		fmt.Println("\n--dry-run: no DMs closed.")
		return nil
	}

	fmt.Print("\nType 'yes' to confirm: ")
	var confirm string
	fmt.Scanln(&confirm)
	if confirm != "yes" {
		fmt.Println("Cancelled.")
		return nil
	}

	closed, failed := 0, 0
	for _, t := range targets {
		if err := c.CloseDM(t.ID); err != nil {
			fmt.Printf("  FAIL %s: %v\n", t.ID, err)
			failed++
		} else {
			fmt.Printf("  CLOSED %s\n", t.ID)
			closed++
		}
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Printf("\n%d closed, %d failed\n", closed, failed)
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sanitize(s string) string {
	r := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", " ", "_",
		"#", "", "@", "",
	)
	return r.Replace(s)
}

func init() {
	// export flags
	cmdExport.Flags().StringSliceVar(&searchGuilds, "guild", nil, "server ID to export from (repeatable)")
	cmdExport.Flags().StringSliceVar(&searchChannels, "channel", nil, "channel/DM ID to export from (repeatable)")
	cmdExport.Flags().StringVar(&exportOutput, "output", "", "output directory (required)")
	cmdExport.Flags().StringVar(&beforeDate, "before", "", "only messages before date (YYYY-MM-DD)")
	cmdExport.Flags().StringVar(&afterDate, "after", "", "only messages after date (YYYY-MM-DD)")
	cmdExport.Flags().IntVar(&exportPerChan, "per-channel", 5000, "max messages per channel (0=unlimited)")
	rootCmd.AddCommand(cmdExport)

	// leave flags
	cmdLeave.Flags().StringSliceVar(&searchGuilds, "guild", nil, "server ID to leave (repeatable)")
	cmdLeave.Flags().IntVar(&leaveInactive, "inactive", 0, "only leave servers inactive > N days")
	cmdLeave.Flags().BoolVar(&dryRun, "dry-run", false, "preview without leaving")
	rootCmd.AddCommand(cmdLeave)

	// close-dms flags
	cmdCloseDMs.Flags().IntVar(&closeInactive, "inactive", 0, "only close DMs inactive > N days")
	cmdCloseDMs.Flags().BoolVar(&dryRun, "dry-run", false, "preview without closing")
	rootCmd.AddCommand(cmdCloseDMs)
}
