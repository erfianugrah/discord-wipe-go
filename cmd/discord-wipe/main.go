// Command discord-wipe is a rolling-retention bulk deleter for your own
// Discord messages. It deletes everything you posted older than a configurable
// number of days, with a search-and-preview step before destructive actions.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/erfianugrah/discord-wipe-go/internal/discord"
	"github.com/erfianugrah/discord-wipe-go/internal/state"
)

var version = "1.0.0"

var (
	token          string
	statePath      string
	exportDir      string
	retentionDays  float64
	deleteDelay    float64
	searchDelay    float64
	intervalHours  float64
	watch          bool
	dryRun         bool
	excludeGuilds  []string
	excludeChans   []string
	ntfyURL        string
	metricsBind    string
	metricsEnabled bool

	// search/purge shared
	searchGuilds   []string
	searchChannels []string
	searchContent  string
	channelFilter  string
	beforeDate     string
	afterDate      string
)

var rootCmd = &cobra.Command{
	Use:   "discord-wipe",
	Short: "Rolling-retention bulk delete for your own Discord messages.",
	Long: `discord-wipe deletes every message you've posted older than a
configurable number of days. Two-phase engine: export reader + live
search-API sweeps. Runs forever as a daemon or one-shot targeted wipe.`,
	Version: version,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&token, "token", os.Getenv("DISCORD_TOKEN"), "Discord user token ($DISCORD_TOKEN)")

	rootCmd.AddCommand(cmdVerify)
	rootCmd.AddCommand(cmdDiscover)
	rootCmd.AddCommand(cmdStatus)
	rootCmd.AddCommand(cmdSeed)
	rootCmd.AddCommand(cmdSearch)
	rootCmd.AddCommand(cmdPurge)
	rootCmd.AddCommand(cmdRun)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// newClient creates a Discord client, fatally exiting on empty token.
func newClient() *discord.Client {
	if token == "" {
		fmt.Fprintln(os.Stderr, "ERROR: no token. Set DISCORD_TOKEN env var or pass --token.")
		os.Exit(2)
	}
	return discord.NewClient(token)
}

// newState creates or loads a state file.
func newState() *state.State {
	s, err := state.New(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] state unwritable: %v\n", err)
		os.Exit(1)
	}
	return s
}

// signalCtx returns a context cancelled on SIGINT or SIGTERM.
func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("received signal, shutting down")
		cancel()
	}()
	return ctx, cancel
}
