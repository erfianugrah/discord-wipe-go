package export

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFixture builds a minimal Discord export tree under a temp dir.
func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "index.json"), `{"111":"my-guild #general","222":"DM with bob"}`)

	mkChan := func(id, typ, msgs string) {
		cd := filepath.Join(dir, "c"+id)
		if err := os.MkdirAll(cd, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(cd, "channel.json"), `{"type":"`+typ+`"}`)
		mustWrite(t, filepath.Join(cd, "messages.json"), msgs)
	}
	mkChan("111", "GUILD_TEXT", `[{"ID":"900","Timestamp":"2024-01-02 03:04:05","Content":"hi"},{"ID":"901","Timestamp":"2025-06-01 00:00:00","Content":"bye"}]`)
	mkChan("222", "DM", `[{"ID":"902","Timestamp":"2023-12-31 23:59:59","Content":"dm"}]`)
	// A non-channel dir that must be ignored.
	if err := os.MkdirAll(filepath.Join(dir, "notachannel"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadExportListsOnlyChannelDirs(t *testing.T) {
	dir := writeFixture(t)
	chans, err := ReadExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 2 {
		t.Fatalf("got %d channels, want 2 (the 'notachannel' dir must be ignored)", len(chans))
	}
	byID := map[string]Channel{}
	for _, c := range chans {
		byID[c.ID] = c
	}
	if byID["111"].Name != "my-guild #general" || byID["111"].MsgCount != 2 {
		t.Fatalf("channel 111 = %+v, want name from index.json and MsgCount 2", byID["111"])
	}
	if byID["222"].Type != "DM" {
		t.Fatalf("channel 222 type = %q, want DM", byID["222"].Type)
	}
}

// SAFETY MANDATE 3 (only-my-messages): the export reader only ever reads
// c<id>/messages.json, which by definition contains only the requester's own
// messages. It must never enumerate "all messages" via any other path.
func TestReadChannelMessagesParsesOurMessagesOnly(t *testing.T) {
	dir := writeFixture(t)
	msgs, err := ReadChannelMessages(filepath.Join(dir, "c111", "messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ID != "900" || msgs[1].Content != "bye" {
		t.Fatalf("unexpected parse: %+v", msgs)
	}
}

func TestReadChannelMessagesCorruptIsErrorNotPanic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "messages.json")
	mustWrite(t, p, `{ this is not valid json`)
	if _, err := ReadChannelMessages(p); err == nil {
		t.Fatal("expected error on corrupt messages.json, got nil")
	}
}

func TestParseTimestampIsUTC(t *testing.T) {
	ts, err := ParseTimestamp("2024-01-02 03:04:05")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if !ts.Equal(want) {
		t.Fatalf("got %s, want %s", ts, want)
	}
	if _, off := ts.Zone(); off != 0 {
		t.Fatalf("timestamp not UTC: offset %d", off)
	}
}
