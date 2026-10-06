// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package snapshotcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type entry struct {
	Digests map[string]string `json:"digests"`
	Bytes   int64             `json:"bytes"`
}

type Store struct {
	root     string
	maxBytes int64
	mutex    sync.RWMutex
}

func New(root string, maxBytes int64) *Store { return &Store{root: root, maxBytes: maxBytes} }

func (store *Store) directory(uri string) string {
	digest := sha256.Sum256([]byte(uri))
	return filepath.Join(store.root, hex.EncodeToString(digest[:]))
}

func (store *Store) metadata(uri string) (entry, error) {
	var result entry
	data, err := os.ReadFile(filepath.Join(store.directory(uri), "checksums.json"))
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(data, &result)
	return result, err
}

func (store *Store) Manifest(uri string) ([]byte, error) {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	metadata, err := store.metadata(uri)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(store.directory(uri), "manifest.json"))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != metadata.Digests["manifest.json"] {
		return nil, fmt.Errorf("cached manifest checksum mismatch")
	}
	return data, nil
}

func safeName(name string) bool {
	return name != "" && name != "." && filepath.Base(name) == name && !strings.HasPrefix(name, ".")
}

func copyFile(source, destination string) (string, int64, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", 0, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("snapshot cache accepts regular files only")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	digest := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(output, digest), input)
	closeErr := output.Close()
	return hex.EncodeToString(digest.Sum(nil)), size, errors.Join(copyErr, closeErr)
}

func (store *Store) Restore(ctx context.Context, uri, destination string, files []string) error {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	metadata, err := store.metadata(uri)
	if err != nil {
		return err
	}
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeName(name) {
			return fmt.Errorf("invalid cached snapshot filename")
		}
		digest, _, err := copyFile(filepath.Join(store.directory(uri), name), filepath.Join(destination, name))
		if err != nil {
			return err
		}
		if digest != metadata.Digests[name] {
			return fmt.Errorf("cached snapshot checksum mismatch")
		}
	}
	now := time.Now()
	return os.Chtimes(store.directory(uri), now, now)
}

func (store *Store) Install(ctx context.Context, uri, source string, files []string, manifest []byte) error {
	if store.maxBytes <= 0 {
		return nil
	}
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(store.root, ".snapshot-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	metadata := entry{Digests: map[string]string{}, Bytes: int64(len(manifest))}
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeName(name) || name == "manifest.json" || name == "checksums.json" {
			return fmt.Errorf("invalid snapshot cache filename")
		}
		digest, size, err := copyFile(filepath.Join(source, name), filepath.Join(temporary, name))
		if err != nil {
			return err
		}
		metadata.Digests[name] = digest
		metadata.Bytes += size
		if metadata.Bytes > store.maxBytes {
			return nil
		}
	}
	if err := os.WriteFile(filepath.Join(temporary, "manifest.json"), manifest, 0o600); err != nil {
		return err
	}
	digest := sha256.Sum256(manifest)
	metadata.Digests["manifest.json"] = hex.EncodeToString(digest[:])
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temporary, "checksums.json"), data, 0o600); err != nil {
		return err
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if err := os.RemoveAll(store.directory(uri)); err != nil {
		return err
	}
	if err := os.Rename(temporary, store.directory(uri)); err != nil {
		return err
	}
	return store.evict()
}

func (store *Store) evict() error {
	entries, err := os.ReadDir(store.root)
	if err != nil {
		return err
	}
	type candidate struct {
		path string
		size int64
		used time.Time
	}
	var candidates []candidate
	var total int64
	for _, directory := range entries {
		if !directory.IsDir() || strings.HasPrefix(directory.Name(), ".") {
			continue
		}
		path := filepath.Join(store.root, directory.Name())
		data, err := os.ReadFile(filepath.Join(path, "checksums.json"))
		if err != nil {
			return err
		}
		var metadata entry
		if err := json.Unmarshal(data, &metadata); err != nil {
			return err
		}
		info, err := directory.Info()
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{path, metadata.Bytes, info.ModTime()})
		total += metadata.Bytes
	}
	sort.Slice(candidates, func(first, second int) bool { return candidates[first].used.Before(candidates[second].used) })
	for _, candidate := range candidates {
		if total <= store.maxBytes {
			break
		}
		if err := os.RemoveAll(candidate.path); err != nil {
			return err
		}
		total -= candidate.size
	}
	return nil
}
