// Package export reads the official Discord data export format.
//
// The export is a directory tree:
//
//	Messages/
//	  index.json          — channel_id → channel_name map
//	  c<channel_id>/
//	    channel.json      — channel metadata (type, name)
//	    messages.json     — array of message objects
//
// Each message has ID, Timestamp, and optional Content/Attachments.
package export

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Channel represents one channel in the export.
type Channel struct {
	ID       string
	Type     string // "DM", "GROUP_DM", "GUILD_TEXT", etc.
	Name     string // human-readable name from index.json
	MsgCount int    // number of messages in the file
}

// Message represents one message in the export.
type Message struct {
	ID        string `json:"ID"`
	Timestamp string `json:"Timestamp"`
	Content   string `json:"Content"`
}

// ReadExport scans the Messages directory and returns all channels.
func ReadExport(dir string) ([]Channel, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("export dir not found: %s", dir)
	}

	// Read index.json for channel names
	indexPath := filepath.Join(dir, "index.json")
	index := make(map[string]string)
	if data, err := os.ReadFile(indexPath); err == nil {
		json.Unmarshal(data, &index) //nolint:errcheck
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read export dir: %w", err)
	}

	var channels []Channel
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "c") {
			continue
		}
		cid := e.Name()[1:] // strip 'c' prefix
		chDir := filepath.Join(dir, e.Name())

		// Read channel.json
		chJSONPath := filepath.Join(chDir, "channel.json")
		var chMeta struct {
			Type string `json:"type"`
		}
		if data, err := os.ReadFile(chJSONPath); err == nil {
			json.Unmarshal(data, &chMeta) //nolint:errcheck
		}

		// Count messages
		msgsPath := filepath.Join(chDir, "messages.json")
		msgCount := 0
		if data, err := os.ReadFile(msgsPath); err == nil {
			var msgs []json.RawMessage
			if json.Unmarshal(data, &msgs) == nil {
				msgCount = len(msgs)
			}
		}

		channels = append(channels, Channel{
			ID:       cid,
			Type:     chMeta.Type,
			Name:     index[cid],
			MsgCount: msgCount,
		})
	}
	return channels, nil
}

// ReadChannelMessages reads and parses messages.json for a single channel.
func ReadChannelMessages(msgsPath string) ([]Message, error) {
	data, err := os.ReadFile(msgsPath)
	if err != nil {
		return nil, err
	}
	var msgs []Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("parse messages.json: %w", err)
	}
	return msgs, nil
}

// ParseTimestamp parses an export timestamp string ("YYYY-MM-DD HH:MM:SS").
func ParseTimestamp(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
}
