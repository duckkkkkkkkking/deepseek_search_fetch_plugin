package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiskStatePersistsAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := openDiskStateStore(dir, 12*time.Hour, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	history := []any{Object{"role": "user", "content": "persisted"}}
	if err := store.remember("resp-1", []Object{{"call_id": "call-1"}},
		"client-a", "model-a", history); err != nil {
		t.Fatal(err)
	}

	reopened, err := openDiskStateStore(dir, 12*time.Hour, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	restored, err := reopened.restore("call-1", "client-a", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(history)
	right, _ := json.Marshal(restored)
	if string(left) != string(right) {
		t.Fatal("disk state changed across reopen")
	}
	if _, err := reopened.restore("call-1", "client-b", "model-a"); !errors.Is(err, errStateForbidden) {
		t.Fatal("state owner isolation was not enforced", err)
	}
}

func TestDiskStateExpiresAfterTwelveHours(t *testing.T) {
	store, err := openDiskStateStore(filepath.Join(t.TempDir(), "state"), 12*time.Hour, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	now := time.Unix(1_800_000_000, 0)
	store.now = func() time.Time { return now }
	if err := store.remember("resp-ttl", nil, "client", "model", []any{Object{
		"role": "user", "content": "expires"}}); err != nil {
		t.Fatal(err)
	}
	if !store.has("resp-ttl") {
		t.Fatal("fresh state was not available")
	}
	store.now = func() time.Time { return now.Add(12*time.Hour + time.Second) }
	if err := store.cleanup(); err != nil {
		t.Fatal(err)
	}
	if store.has("resp-ttl") {
		t.Fatal("expired state was not removed")
	}
}

func TestDiskStateEvictsOldestWithinCapacity(t *testing.T) {
	store, err := openDiskStateStore(filepath.Join(t.TempDir(), "state"), 12*time.Hour, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	now := time.Unix(1_800_000_000, 0)
	store.now = func() time.Time { return now }
	for _, id := range []string{"state-a", "state-b", "state-c"} {
		if err := store.remember(id, nil, "client", "model", []any{Object{
			"role": "user", "content": strings.Repeat("x", 6000)}}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
	}
	if store.has("state-a") {
		t.Fatal("oldest state was not evicted")
	}
	if !store.has("state-c") {
		t.Fatal("newest state was evicted")
	}
}
