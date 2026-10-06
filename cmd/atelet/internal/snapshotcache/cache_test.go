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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestImmutableSnapshotHitCorruptionAndBudget(t *testing.T) {
	cache := New(t.TempDir(), 100)
	source, destination := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "home.tar"), []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"scope":"data"}`)
	uri := "azblob://actors/snapshots/uuid-1"
	if err := cache.Install(t.Context(), uri, source, []string{"home.tar"}, manifest); err != nil {
		t.Fatal(err)
	}
	if data, err := cache.Manifest(uri); err != nil || string(data) != string(manifest) {
		t.Fatalf("manifest = %q, %v", data, err)
	}
	if err := cache.Restore(t.Context(), uri, destination, []string{"home.tar"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.directory(uri), "home.tar"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Restore(t.Context(), uri, destination, []string{"home.tar"}); err == nil {
		t.Fatal("corruption was not detected")
	}
	if _, err := cache.Manifest("azblob://actors/snapshots/uuid-2"); err == nil {
		t.Fatal("another snapshot matched the cache")
	}
	cache.maxBytes = 25
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cache.directory(uri), old, old); err != nil {
		t.Fatal(err)
	}
	if err := cache.Install(t.Context(), "azblob://actors/snapshots/uuid-2", source, []string{"home.tar"}, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Manifest(uri); err == nil {
		t.Fatal("cache did not enforce byte budget")
	}
}
