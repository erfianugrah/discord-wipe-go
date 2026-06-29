package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/erfianugrah/discord-wipe-go/internal/discord"
	"github.com/erfianugrah/discord-wipe-go/internal/snowflake"
	"github.com/erfianugrah/discord-wipe-go/internal/state"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// search
// ---------------------------------------------------------------------------

var cmdSearch = &cobra.Command{
	Use:   "search",
	Short: "Search your messages across servers/DMs and preview results.",
	Long: `Search your messages with an optional content filter and preview
channel + timestamp + content before deciding to delete.
Use this as the discovery step before 'purge'.

Examples:
  discord-wipe search --guild 123456789 --content "bad phrase"
  discord-wipe search --channel 987654321
  discord-wipe search --content "old project name"
  discord-wipe search --guild 123456789 --before 2024-06-01 --after 2024-01-01`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := signalCtx()
		defer cancel()

		c := newClient()
		me, err := c.GetMe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL — %v\n", err)
			os.Exit(2)
		}

		// Build search scopes: (scopeType, scopeID, label, channelFilter)
		type scope struct {
			typ, id, label, chanFilter string
		}
		var scopes []scope

		switch {
		case len(searchChannels) > 0 || len(searchGuilds) > 0:
			// Resolve DM names for channel scopes
			dmMap := map[string]string{}
			if len(searchChannels) > 0 {
				if dms, err := c.ListDMs(); err == nil {
					for _, dm := range dms {
						var names []string
						for _, r := range dm.Recipients {
							names = append(names, r.Username)
						}
						dmMap[dm.ID] = "DM:" + join(names, ",")
					}
				}
			}
			for _, cid := range searchChannels {
				label := dmMap[cid]
				if label == "" {
					label = "channel:" + cid
				}
				scopes = append(scopes, scope{"channel", cid, label, ""})
			}
			for _, gid := range searchGuilds {
				gname := gid
				if g, err := c.GetGuild(gid); err == nil {
					gname = g.Name
				}
				scopes = append(scopes, scope{"guild", gid, gname, channelFilter})
			}
		default:
			// Search everywhere
			guilds, _ := c.ListGuilds()
			dms, _ := c.ListDMs()
			for _, g := range guilds {
				scopes = append(scopes, scope{"guild", g.ID, g.Name, ""})
			}
			for _, dm := range dms {
				var names []string
				for _, r := range dm.Recipients {
					names = append(names, r.Username)
				}
				scopes = append(scopes, scope{"channel", dm.ID, "DM:" + join(names, ","), ""})
			}
		}

		if len(scopes) == 0 {
			fmt.Println("no scopes to search — are you in any servers or DMs?")
			os.Exit(1)
		}

		// Prefetch guild channels for name resolution (parallel)
		guildChannels := sync.Map{} // guildID → map[channelID]name
		var wg sync.WaitGroup
		for _, s := range scopes {
			if s.typ != "guild" {
				continue
			}
			if _, loaded := guildChannels.LoadOrStore(s.id, struct{}{}); loaded {
				continue
			}
			wg.Add(1)
			go func(gid string) {
				defer wg.Done()
				chs, err := c.ListGuildChannels(gid)
				if err != nil {
					guildChannels.Store(gid, map[string]string{})
					return
				}
				m := make(map[string]string, len(chs))
				for _, ch := range chs {
					m[ch.ID] = "#" + ch.Name
				}
				guildChannels.Store(gid, m)
			}(s.id)
		}
		wg.Wait()

		// Snowflake bounds
		maxID := snowflake.At(time.Now().UTC())
		if beforeDate != "" {
			maxID = mustSnowflake(beforeDate)
		}
		minID := int64(0)
		if afterDate != "" {
			minID = mustSnowflake(afterDate)
		}

		// Header
		parts := []string{}
		if searchContent != "" {
			parts = append(parts, fmt.Sprintf("content=%q", searchContent))
		} else {
			parts = append(parts, "recent messages")
		}
		if len(searchGuilds) > 0 {
			parts = append(parts, fmt.Sprintf("%d server(s)", len(searchGuilds)))
		} else if len(searchChannels) > 0 {
			parts = append(parts, fmt.Sprintf("%d channel(s)", len(searchChannels)))
		} else {
			parts = append(parts, fmt.Sprintf("%d scopes", len(scopes)))
		}
		fmt.Printf("Search: %s\n", join(parts, "; "))
		fmt.Printf("  authenticated as @%s\n", me.Username)
		if beforeDate != "" {
			fmt.Printf("  before: %s\n", beforeDate)
		}
		if afterDate != "" {
			fmt.Printf("  after:  %s\n", afterDate)
		}
		fmt.Println()

		grandTotal := 0
		for _, s := range scopes {
			select {
			case <-ctx.Done():
				return
			default:
			}

			params := discord.SearchParams{
				AuthorID:  me.ID,
				MaxID:     maxID,
				MinID:     minID,
				Content:   searchContent,
				ChannelID: s.chanFilter,
			}
			total, hits, retry := c.SearchMessages(s.typ, s.id, params)

			if total < 0 {
				fmt.Printf("[%s] 🔒 no permission\n\n", s.label)
				continue
			}
			if retry > 0 {
				fmt.Printf("[%s] ⏳ rate-limited (retry in %.0fs)\n\n", s.label, retry)
				continue
			}
			if len(hits) == 0 {
				fmt.Printf("[%s] (no matches)\n\n", s.label)
				continue
			}

			statusIcon := ""
			if total > 0 {
				statusIcon = fmt.Sprintf(" (%d shown of %d total)", len(hits), total)
			}
			fmt.Printf("[%s]%s\n", s.label, statusIcon)

			chMap, _ := guildChannels.Load(s.id)
			channels := map[string]string{}
			if m, ok := chMap.(map[string]string); ok {
				channels = m
			}

			for _, m := range hits {
				chName := channels[m.ChannelID]
				if chName == "" {
					chName = m.ChannelID
				}
				ts := snowflake.Timestamp(parseSnowflake(m.ID))
				tsStr := ts.Format("2006-01-02 15:04 UTC")

				content := strings.ReplaceAll(m.Content, "\n", "\\n")
				if len(content) > 140 {
					content = content[:137] + "..."
				}
				if content == "" {
					content = "[attachment / embed]"
				}
				fmt.Printf("  [%s] %s\n", chName, tsStr)
				fmt.Printf("    %s\n", content)
			}
			grandTotal += len(hits)
			fmt.Println()
		}

		fmt.Printf("--- %d matches shown ---\n", grandTotal)
		if len(searchGuilds) > 0 {
			gids := ""
			for _, g := range searchGuilds {
				gids += " --guild " + g
			}
			fmt.Printf("\nTo delete all:  discord-wipe purge%s\n", gids)
		} else if len(searchChannels) > 0 {
			cids := ""
			for _, c := range searchChannels {
				cids += " --channel " + c
			}
			fmt.Printf("\nTo delete all:  discord-wipe purge%s\n", cids)
		} else {
			fmt.Println("\nTo delete from a server:  discord-wipe purge --guild <ID>")
			fmt.Println("To delete from a channel: discord-wipe purge --channel <ID>")
		}
	},
}

// ---------------------------------------------------------------------------
// purge
// ---------------------------------------------------------------------------

var cmdPurge = &cobra.Command{
	Use:   "purge",
	Short: "One-shot targeted wipe of specific guilds/channels.",
	Long: `Delete all your messages in one or more servers or channels without
touching the rest of your account. Retention defaults to 0 —
everything in the target scope is deleted regardless of age.

Use 'search' first to preview what would be deleted.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := signalCtx()
		defer cancel()

		// Re-resolve shared-global flags (see cmdRun.Run). purge's retention
		// defaults to 0 — delete everything in scope, regardless of age — and is
		// intentionally env-independent so a stray RETENTION_DAYS can't narrow it.
		retentionDays = resolveFloat(cmd, "retention-days", "", 0)
		deleteDelay = resolveFloat(cmd, "delete-delay", "DELETE_DELAY", 1.0)
		searchDelay = resolveFloat(cmd, "search-delay", "SEARCH_DELAY", 15.0)
		statePath = resolveString(cmd, "state", "STATE_PATH", "/data/state/state.json")

		if len(searchGuilds) == 0 && len(searchChannels) == 0 {
			fmt.Fprintln(os.Stderr, "ERROR: specify at least one --guild GUILD_ID or --channel CHANNEL_ID")
			os.Exit(2)
		}

		c := newClient()
		me, err := c.GetMe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL — %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("[purge] authenticated as @%s (id=%s)\n", me.Username, me.ID)

		s := newState()
		fmt.Printf("[purge] state: %s (%d IDs already done; export_consumed=%v)\n",
			statePath, s.Len(), s.ExportConsumed)

		cutoff := retentionCutoff(time.Now().UTC(), retentionDays)
		cutoffSF := snowflake.At(cutoff)

		// Build targets
		var targets []ScopedTarget
		for _, gid := range searchGuilds {
			targets = append(targets, ScopedTarget{"guild", gid, "guild:" + gid})
		}
		for _, cid := range searchChannels {
			targets = append(targets, ScopedTarget{"channel", cid, "channel:" + cid})
		}

		var labels []string
		for _, t := range targets {
			labels = append(labels, t.Label)
		}
		dryLabel := ""
		if dryRun {
			dryLabel = " — DRY RUN"
		}
		fmt.Printf("[purge] targets: %v\n[purge] cutoff: %s (retention_days=%.0f)%s\n",
			labels, cutoff.Format(time.RFC3339), retentionDays, dryLabel)

		t0 := time.Now()
		counts := liveCatchup(ctx, c, me.ID, s, targets, cutoffSF, dryRun)
		s.LastPassAt = time.Now().UTC().Format(time.RFC3339)
		s.Save() //nolint:errcheck
		elapsed := time.Since(t0)
		fmt.Printf("[purge] === done in %s (%.0fs) === ok=%d gone=%d forbidden=%d\n",
			durStr(elapsed), elapsed.Seconds(), counts.ok, counts.gone, counts.forbidden)
	},
}

// ---------------------------------------------------------------------------
// live catchup engine (shared by purge and run)
// ---------------------------------------------------------------------------

// ScopedTarget is a wipe target: a guild or channel scope with a label.
type ScopedTarget struct {
	Scope, ID, Label string
}

type catchupCounts struct {
	ok, gone, forbidden int
}

func liveCatchup(ctx context.Context, c *discord.Client, meID string, st *state.State,
	targets []ScopedTarget, cutoffSF int64, dryRun bool) catchupCounts {

	var counts catchupCounts
	searchD := time.Duration(searchDelay * float64(time.Second))
	deleteD := time.Duration(deleteDelay * float64(time.Second))

	for ti, t := range targets {
		select {
		case <-ctx.Done():
			return counts
		default:
		}
		fmt.Printf("[catchup %d/%d] %s\n", ti+1, len(targets), t.Label)

		extraSleep := 0.0
		floor429 := 0.0
		emptyStreak := 0

		for {
			select {
			case <-ctx.Done():
				return counts
			default:
			}

			params := discord.SearchParams{
				AuthorID: meID,
				MaxID:    cutoffSF,
			}
			total, hits, retry := c.SearchMessages(t.Scope, t.ID, params)
			if retry > 0 {
				fmt.Printf("  rate-limited / index lag; sleep %.1fs\n", retry)
				time.Sleep(time.Duration(retry * float64(time.Second)))
				continue
			}
			if total < 0 {
				fmt.Println("  no permission to search this scope; skipping")
				break
			}
			if len(hits) == 0 {
				emptyStreak++
				if emptyStreak >= 2 {
					break
				}
				time.Sleep(searchD)
				continue
			}
			emptyStreak = 0

			// No-progress guard
			newInPage := 0
			for _, m := range hits {
				if !st.IsDeleted(m.ID) {
					newInPage++
				}
			}
			if newInPage == 0 {
				fmt.Printf("  page: %d hits, all already done; scope finished\n", len(hits))
				break
			}
			fmt.Printf("  page: %d hits (%d new, search reports total=%d)\n", len(hits), newInPage, total)

			for _, m := range hits {
				select {
				case <-ctx.Done():
					return counts
				default:
				}
				if st.IsDeleted(m.ID) {
					continue
				}
				if dryRun {
					counts.ok++
					st.Mark(m.ID)
					continue
				}

				for {
					result := c.DeleteMessage(m.ChannelID, m.ID)
					switch result.Status {
					case "retry":
						fmt.Printf("    rate-limited; sleep %.1fs\n", result.RetryAfter)
						extraSleep = result.RetryAfter
						floor429 = result.RetryAfter
						time.Sleep(time.Duration(result.RetryAfter * float64(time.Second)))
						continue
					case "ok":
						counts.ok++
						if result.RetryAfter > 0 {
							extraSleep = result.RetryAfter
							if extraSleep < floor429 {
								extraSleep = floor429
							}
						}
					case "gone":
						counts.gone++
						if result.RetryAfter > 0 {
							extraSleep = result.RetryAfter
							if extraSleep < floor429 {
								extraSleep = floor429
							}
						}
					case "forbidden":
						counts.forbidden++
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
			time.Sleep(searchD)
		}
	}
	return counts
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseSnowflake(s string) int64 {
	var sf int64
	fmt.Sscanf(s, "%d", &sf)
	return sf
}

func durStr(d time.Duration) string {
	secs := d.Seconds()
	switch {
	case secs < 90:
		return fmt.Sprintf("%.0fs", secs)
	case secs < 3600:
		return fmt.Sprintf("%.0fm%.0fs", secs/60, float64(int(secs)%60))
	case secs < 86400:
		return fmt.Sprintf("%.0fh%.0fm", secs/3600, float64(int(secs)%3600)/60)
	default:
		days := secs / 86400
		hours := float64(int(secs)%86400) / 3600
		return fmt.Sprintf("%.0fd%.0fh", days, hours)
	}
}

func init() {
	for _, c := range []*cobra.Command{cmdSearch, cmdPurge} {
		c.Flags().StringSliceVar(&searchGuilds, "guild", nil, "server ID (repeatable)")
		c.Flags().StringSliceVar(&searchChannels, "channel", nil, "channel/DM ID (repeatable)")
	}
	cmdSearch.Flags().StringVar(&channelFilter, "channel-filter", "", "limit guild search to this channel")
	cmdSearch.Flags().StringVar(&searchContent, "content", "", "text to search for")
	cmdSearch.Flags().StringVar(&beforeDate, "before", "", "messages before date (YYYY-MM-DD)")
	cmdSearch.Flags().StringVar(&afterDate, "after", "", "messages after date (YYYY-MM-DD)")

	cmdPurge.Flags().StringVar(&statePath, "state", "/data/state/state.json", "state file path")
	cmdPurge.Flags().Float64Var(&retentionDays, "retention-days", 0, "only delete messages older than N days")
	cmdPurge.Flags().Float64Var(&deleteDelay, "delete-delay", 1.0, "seconds between DELETE calls")
	cmdPurge.Flags().Float64Var(&searchDelay, "search-delay", 15.0, "seconds between search page fetches")
	cmdPurge.Flags().BoolVar(&dryRun, "dry-run", false, "report without deleting")
}
