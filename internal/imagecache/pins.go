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

package imagecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

type imagePin struct {
	Digest    string    `json:"digest"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (store *Store) EnsureImagePinned(ctx context.Context, reference, owner string, ttl time.Duration) (*Image, error) {
	if owner == "" || ttl <= 0 || ttl > 24*time.Hour {
		return nil, fmt.Errorf("image pin requires an owner and a TTL up to 24 hours")
	}
	image, err := store.EnsureImage(ctx, reference)
	if err != nil {
		return nil, err
	}
	store.hitMu.Lock()
	defer store.hitMu.Unlock()
	available, err := store.cachedImage(image.Digest)
	if err != nil {
		return nil, err
	}
	if available == nil {
		return nil, fmt.Errorf("image was evicted before its preload pin could be committed")
	}
	directory := filepath.Join(store.root, "pins")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(directory, ".pin-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	writeErr := json.NewEncoder(file).Encode(imagePin{Digest: image.Digest.String(), ExpiresAt: time.Now().Add(ttl)})
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return nil, err
	}
	ownerHash := sha256.Sum256([]byte(owner))
	if err := os.Rename(file.Name(), filepath.Join(directory, hex.EncodeToString(ownerHash[:])+".json")); err != nil {
		return nil, err
	}
	return image, nil
}

func (store *Store) addPinRoots(roots *RootSet) error {
	directory := filepath.Join(store.root, "pins")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		var pin imagePin
		if err := json.Unmarshal(data, &pin); err != nil {
			return err
		}
		if !pin.ExpiresAt.After(time.Now()) {
			continue
		}
		digest, err := v1.NewHash(pin.Digest)
		if err != nil {
			return err
		}
		roots.ImageDigests[pin.Digest] = true
		image, err := store.cachedImage(digest)
		if err != nil {
			return err
		}
		if image != nil {
			addImageRoots(roots, pin.Digest, image.LayerDirs, "preload-pin", false)
		}
	}
	return nil
}
