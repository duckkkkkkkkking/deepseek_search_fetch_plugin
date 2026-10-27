package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const stateCleanupInterval = 10 * time.Minute

var (
	errStateUnavailable = errors.New("conversation state is unavailable")
	errStateForbidden   = errors.New("conversation state belongs to another client or model")
)

type persistedState struct {
	Owner   string `json:"owner"`
	Model   string `json:"model"`
	History []any  `json:"history"`
}

type diskStateFile struct {
	path string
	size int64
	used time.Time
}

type diskAliasFile struct {
	path   string
	size   int64
	target string
}

type diskStateStore struct {
	dir        string
	entriesDir string
	aliasesDir string
	ttl        time.Duration
	maxBytes   int64
	now        func() time.Time

	mu       sync.Mutex
	stop     chan struct{}
	stopOnce sync.Once
	stopWG   sync.WaitGroup
}

func defaultStateDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("cliproxy-deepseek-search-state-%d", os.Getuid()))
}

func openDiskStateStore(dir string, ttl time.Duration, maxBytes int64) (*diskStateStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("state_dir must be an absolute path")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("state_seconds must be positive")
	}
	if maxBytes < 1024 {
		return nil, fmt.Errorf("state_max_bytes must be at least 1024")
	}
	store := &diskStateStore{
		dir: dir, entriesDir: filepath.Join(dir, "entries"), aliasesDir: filepath.Join(dir, "aliases"),
		ttl: ttl, maxBytes: maxBytes, now: time.Now, stop: make(chan struct{}),
	}
	for _, path := range []string{store.dir, store.entriesDir, store.aliasesDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
		if err := os.Chmod(path, 0700); err != nil {
			return nil, fmt.Errorf("secure state directory: %w", err)
		}
	}
	if err := store.cleanup(); err != nil {
		return nil, err
	}
	store.startCleanup()
	return store, nil
}

func (s *diskStateStore) startCleanup() {
	s.stopWG.Add(1)
	go func() {
		defer s.stopWG.Done()
		ticker := time.NewTicker(stateCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = s.cleanup()
			case <-s.stop:
				return
			}
		}
	}()
}

func (s *diskStateStore) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.stopWG.Wait()
}

func (s *diskStateStore) entryPath(id string) string {
	digest := sha256.Sum256([]byte(id))
	return filepath.Join(s.entriesDir, hex.EncodeToString(digest[:])+".json")
}

func (s *diskStateStore) aliasPath(alias string) string {
	digest := sha256.Sum256([]byte(alias))
	return filepath.Join(s.aliasesDir, hex.EncodeToString(digest[:])+".alias")
}

func (s *diskStateStore) remember(id string, external []Object, owner, model string, history []any) error {
	state := persistedState{Owner: owner, Model: model, History: history}
	s.mu.Lock()
	defer s.mu.Unlock()
	entryPath := s.entryPath(id)
	if err := writeJSONAtomic(s.entriesDir, filepath.Base(entryPath), state); err != nil {
		return err
	}
	target := filepath.Base(entryPath)
	aliases := []string{id}
	for _, call := range external {
		aliases = append(aliases, str(call["call_id"]))
	}
	for _, alias := range uniqueStrings(aliases) {
		if err := writeFileAtomic(s.aliasesDir, filepath.Base(s.aliasPath(alias)), []byte(target)); err != nil {
			return err
		}
	}
	now := s.now()
	_ = os.Chtimes(entryPath, now, now)
	if err := s.cleanupLocked(); err != nil {
		return err
	}
	if _, err := os.Stat(s.aliasPath(id)); err != nil {
		return fmt.Errorf("conversation state exceeds state_max_bytes")
	}
	return nil
}

func (s *diskStateStore) restore(alias, client, model string) ([]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rawTarget, err := os.ReadFile(s.aliasPath(alias))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errStateUnavailable
	}
	if err != nil {
		return nil, err
	}
	target := string(rawTarget)
	if filepath.Base(target) != target {
		return nil, errStateUnavailable
	}
	path := filepath.Join(s.entriesDir, target)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errStateUnavailable
	}
	if err != nil {
		return nil, err
	}
	if s.now().Sub(info.ModTime()) >= s.ttl {
		_ = os.Remove(path)
		return nil, errStateUnavailable
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errStateUnavailable
	}
	var state persistedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, errStateUnavailable
	}
	if state.Owner == "" || state.Owner != client || state.Model != model {
		return nil, errStateForbidden
	}
	now := s.now()
	_ = os.Chtimes(path, now, now)
	return state.History, nil
}

func (s *diskStateStore) has(alias string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rawTarget, err := os.ReadFile(s.aliasPath(alias))
	if err != nil {
		return false
	}
	target := string(rawTarget)
	if filepath.Base(target) != target {
		return false
	}
	info, err := os.Stat(filepath.Join(s.entriesDir, target))
	return err == nil && s.now().Sub(info.ModTime()) < s.ttl
}

func (s *diskStateStore) cleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupLocked()
}

func (s *diskStateStore) cleanupLocked() error {
	entries, aliases, total, err := s.scanFilesLocked()
	if err != nil {
		return err
	}
	now := s.now()
	for name, file := range entries {
		if now.Sub(file.used) >= s.ttl {
			if err := os.Remove(file.path); err == nil {
				total -= file.size
				delete(entries, name)
			}
		}
	}
	for name, alias := range aliases {
		if _, exists := entries[alias.target]; !exists {
			if err := os.Remove(alias.path); err == nil {
				total -= alias.size
				delete(aliases, name)
			}
		}
	}
	if total <= s.maxBytes {
		return nil
	}
	ordered := make([]*diskStateFile, 0, len(entries))
	for _, file := range entries {
		ordered = append(ordered, file)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].used.Equal(ordered[j].used) {
			return ordered[i].path < ordered[j].path
		}
		return ordered[i].used.Before(ordered[j].used)
	})
	for _, entry := range ordered {
		if total <= s.maxBytes {
			break
		}
		name := filepath.Base(entry.path)
		if err := os.Remove(entry.path); err == nil {
			total -= entry.size
			for aliasName, alias := range aliases {
				if alias.target != name {
					continue
				}
				if err := os.Remove(alias.path); err == nil {
					total -= alias.size
					delete(aliases, aliasName)
				}
			}
		}
	}
	return nil
}

func (s *diskStateStore) scanFilesLocked() (map[string]*diskStateFile, map[string]*diskAliasFile, int64, error) {
	entries := make(map[string]*diskStateFile)
	aliases := make(map[string]*diskAliasFile)
	total := int64(0)
	entryRows, err := os.ReadDir(s.entriesDir)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, row := range entryRows {
		info, err := row.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
		if strings.HasPrefix(row.Name(), ".tmp-") {
			if err := os.Remove(filepath.Join(s.entriesDir, row.Name())); err == nil {
				total -= info.Size()
			}
			continue
		}
		entries[row.Name()] = &diskStateFile{path: filepath.Join(s.entriesDir, row.Name()), size: info.Size(), used: info.ModTime()}
	}
	aliasRows, err := os.ReadDir(s.aliasesDir)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, row := range aliasRows {
		info, err := row.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
		if strings.HasPrefix(row.Name(), ".tmp-") {
			if err := os.Remove(filepath.Join(s.aliasesDir, row.Name())); err == nil {
				total -= info.Size()
			}
			continue
		}
		path := filepath.Join(s.aliasesDir, row.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		target := string(raw)
		if filepath.Base(target) != target {
			_ = os.Remove(path)
			total -= info.Size()
			continue
		}
		aliases[row.Name()] = &diskAliasFile{path: path, size: info.Size(), target: target}
	}
	return entries, aliases, total, nil
}

func writeJSONAtomic(dir, name string, value any) error {
	file, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(value); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(dir, name))
}

func writeFileAtomic(dir, name string, raw []byte) error {
	file, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(dir, name))
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
