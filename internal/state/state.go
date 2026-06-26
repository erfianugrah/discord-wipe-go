// Package state manages durable JSON state for the wipe tool.
//
// It tracks deleted message IDs, the export-consumed flag, restart-burst
// counters, and a heartbeat file for docker HEALTHCHECK. Writes are atomic
// (tmp + fsync + rename) with a .bak fallback, matching the Python version's
// Bug12 durability guarantees.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// State is the on-disk state for a wipe run.
type State struct {
	path          string
	backupPath    string
	heartbeatPath string

	Deleted        map[string]bool `json:"deleted"`
	ExportConsumed bool            `json:"export_consumed"`
	LastPassAt     string          `json:"last_pass_at,omitempty"`
	LastStartedAt  string          `json:"last_started_at,omitempty"`
	RestartBurst   int             `json:"restart_burst"`
}

// New creates or loads state at the given file path.
func New(path string) (*State, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("state directory %s is unwritable: %w", dir, err)
		}
	}
	s := &State{
		path:          path,
		backupPath:    path + ".bak",
		heartbeatPath: filepath.Join(dir, "heartbeat"),
		Deleted:       make(map[string]bool),
	}
	s.load()
	return s, nil
}

func (s *State) load() {
	for _, candidate := range []string{s.path, s.backupPath} {
		data, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var raw struct {
			Deleted        []string `json:"deleted"`
			ExportConsumed bool     `json:"export_consumed"`
			LastPassAt     string   `json:"last_pass_at"`
			LastStartedAt  string   `json:"last_started_at"`
			RestartBurst   int      `json:"restart_burst"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			s.quarantine(candidate, err)
			continue
		}
		for _, id := range raw.Deleted {
			s.Deleted[id] = true
		}
		s.ExportConsumed = raw.ExportConsumed
		s.LastPassAt = raw.LastPassAt
		s.LastStartedAt = raw.LastStartedAt
		s.RestartBurst = raw.RestartBurst
		return
	}
}

func (s *State) quarantine(candidate string, err error) {
	ts := time.Now().UTC().Format("20060102T150405Z")
	backup := fmt.Sprintf("%s.corrupt-%s", filepath.Base(candidate), ts)
	backup = filepath.Join(filepath.Dir(s.path), backup)
	if e := os.Rename(candidate, backup); e != nil {
		fmt.Fprintf(os.Stderr, "[state] WARN: %s is corrupt (%v); could not back up (%v)\n",
			candidate, err, e)
		return
	}
	fmt.Fprintf(os.Stderr, "[state] WARN: %s is corrupt (%v); moved to %s\n",
		candidate, err, backup)
}

// Save writes the state durably to disk.
func (s *State) Save() error {
	ids := make([]string, 0, len(s.Deleted))
	for id := range s.Deleted {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	payload := struct {
		Deleted        []string `json:"deleted"`
		ExportConsumed bool     `json:"export_consumed"`
		LastPassAt     string   `json:"last_pass_at,omitempty"`
		LastStartedAt  string   `json:"last_started_at,omitempty"`
		RestartBurst   int      `json:"restart_burst"`
	}{
		Deleted:        ids,
		ExportConsumed: s.ExportConsumed,
		LastPassAt:     s.LastPassAt,
		LastStartedAt:  s.LastStartedAt,
		RestartBurst:   s.RestartBurst,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync tmp: %w", err)
	}
	f.Close()

	// Rotate the previous good copy to .bak before swapping.
	if _, err := os.Stat(s.path); err == nil {
		os.Rename(s.path, s.backupPath) //nolint:errcheck // best-effort
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename tmp -> state: %w", err)
	}

	// Fsync the directory so the rename is durable.
	if df, err := os.Open(filepath.Dir(s.path)); err == nil {
		df.Sync() //nolint:errcheck
		df.Close()
	}

	s.touchHeartbeat()
	return nil
}

func (s *State) touchHeartbeat() {
	os.WriteFile(s.heartbeatPath, []byte(s.LastPassAt), 0o644) //nolint:errcheck
}

// Mark records a message ID as deleted.
func (s *State) Mark(msgID string) {
	s.Deleted[msgID] = true
}

// IsDeleted checks whether a message ID has already been deleted.
func (s *State) IsDeleted(msgID string) bool {
	return s.Deleted[msgID]
}

// Len returns the count of tracked deleted IDs.
func (s *State) Len() int {
	return len(s.Deleted)
}
