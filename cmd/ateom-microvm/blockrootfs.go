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

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/reaper"
	"github.com/agent-substrate/substrate/internal/imagecache"
	"golang.org/x/sys/unix"
)

func defaultRootfsBackend() string {
	if backend := os.Getenv("ATE_MICROVM_ROOTFS_BACKEND"); backend != "" {
		return backend
	}
	return "virtio-fs"
}

func blockRootfsKey(bundle string, sizeMiB int) (string, error) {
	spec, err := imagecache.ReadSpec(bundle)
	if err != nil {
		return "", err
	}
	if spec == nil || !strings.HasPrefix(spec.ImageDigest, "sha256:") || sizeMiB < 32 || sizeMiB > 32768 {
		return "", fmt.Errorf("block rootfs requires a digest-backed overlay and image size between 32 and 32768 MiB")
	}
	extras := slices.Clone(spec.ExtraDirs)
	slices.Sort(extras)
	root, err := os.OpenRoot(filepath.Join(bundle, "rootfs"))
	if err != nil {
		return "", err
	}
	defer root.Close()
	dns, err := root.ReadFile("etc/resolv.conf")
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Version     int
		Digest      string
		Directories []string
		DNS         string
		SizeMiB     int
	}{1, spec.ImageDigest, extras, string(dns), sizeMiB})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func validBlockRootfs(path string, sizeBytes int64) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != sizeBytes {
		return false
	}
	magic := make([]byte, 2)
	_, err = file.ReadAt(magic, 1024+56)
	return err == nil && magic[0] == 0x53 && magic[1] == 0xef
}

func waitBlockRootfsLock(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func ensureBlockRootfs(ctx context.Context, cacheDir, bundle, mkfs string, sizeMiB int) (string, error) {
	key, err := blockRootfsKey(bundle, sizeMiB)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(filepath.Join(cacheDir, key+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := waitBlockRootfsLock(ctx); err != nil {
			return "", err
		}
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	path := filepath.Join(cacheDir, key+".ext4")
	size := int64(sizeMiB) * 1024 * 1024
	if validBlockRootfs(path, size) {
		return path, nil
	}
	file, err := os.CreateTemp(cacheDir, ".block-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	if err := file.Truncate(size); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, mkfs, "-q", "-F", "-t", "ext4", "-b", "4096", "-m", "0", "-O", "^has_journal", "-E", "lazy_itable_init=0", "-d", filepath.Join(bundle, "rootfs"), file.Name())
	var stderr strings.Builder
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := reaper.Run(cmd); err != nil {
		return "", fmt.Errorf("building block rootfs: %w (%s)", err, stderr.String())
	}
	if !validBlockRootfs(file.Name(), size) {
		return "", fmt.Errorf("mkfs did not produce the expected ext4 rootfs")
	}
	if err := os.Chmod(file.Name(), 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
