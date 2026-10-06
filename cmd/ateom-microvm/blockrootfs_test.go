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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/imagecache"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"golang.org/x/sys/unix"
)

func blockRootfsTestBundle(t *testing.T) string {
	t.Helper()
	bundle := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bundle, "rootfs", "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "rootfs", "etc", "resolv.conf"), []byte("nameserver 169.254.17.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := imagecache.WriteSpec(bundle, &imagecache.OverlaySpec{Version: 1, ImageDigest: "sha256:" + strings.Repeat("a", 64), Layers: []string{}}); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestBlockRootfsCache(t *testing.T) {
	bundle := blockRootfsTestBundle(t)
	source := filepath.Join(bundle, "rootfs", "module.py")
	if err := os.WriteFile(source, []byte("value = 1\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	timestamp := time.Unix(1791219949, 123456789)
	if err := os.Chtimes(source, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(bundle, "rootfs", "alias.py")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("module.py", filepath.Join(bundle, "rootfs", "link.py")); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	image, err := ensureBlockRootfs(context.Background(), cache, bundle, "/usr/sbin/mkfs.ext4", 32)
	if err != nil {
		t.Fatal(err)
	}
	if !validBlockRootfs(image, 32*1024*1024) {
		t.Fatal("invalid block image")
	}
	info, err := os.Stat(image)
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("block image permissions: %v, %v", info, err)
	}
	if reused, err := ensureBlockRootfs(context.Background(), cache, bundle, "missing-mkfs", 32); err != nil || reused != image {
		t.Fatalf("warm cache invoked mkfs: %q, %v", reused, err)
	}
	output, err := exec.Command("/usr/sbin/debugfs", "-R", "stat /module.py", image).CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Mode:  0444") || !strings.Contains(string(output), fmt.Sprintf("0x%x", timestamp.Unix())) {
		t.Fatalf("image lost file metadata: %s, %v", output, err)
	}
	alias, err := exec.Command("/usr/sbin/debugfs", "-R", "stat /alias.py", image).CombinedOutput()
	if err != nil || !strings.Contains(string(alias), "Links: 2") {
		t.Fatalf("image lost hardlink: %s, %v", alias, err)
	}
	link, err := exec.Command("/usr/sbin/debugfs", "-R", "stat /link.py", image).CombinedOutput()
	if err != nil || !strings.Contains(string(link), "module.py") {
		t.Fatalf("image lost symlink: %s, %v", link, err)
	}
	key, err := blockRootfsKey(bundle, 64)
	if err != nil || filepath.Base(image) == key+".ext4" {
		t.Fatal("image size did not affect cache identity")
	}
}

func TestBlockRootfsCacheFailureDoesNotPublish(t *testing.T) {
	bundle := blockRootfsTestBundle(t)
	cache := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := ensureBlockRootfs(ctx, cache, bundle, "missing-mkfs", 32); err == nil {
		t.Fatal("missing mkfs accepted")
	}
	images, err := filepath.Glob(filepath.Join(cache, "*.ext4"))
	if err != nil || len(images) != 0 {
		t.Fatalf("failed image published: %v, %v", images, err)
	}
}

func TestBlockRootfsRejectsFullBeforeLifecycleChanges(t *testing.T) {
	service := &AteomService{blockRootfs: true}
	root := t.TempDir()
	dirs := &ateompb.ActorDirs{RootDir: root, OciBundleDir: filepath.Join(root, "bundles"), CheckpointDir: filepath.Join(root, "checkpoint"), RestoreDir: filepath.Join(root, "restore"),
		DurableDirVolumeMountsDir: filepath.Join(root, "durable"), SystemInfoVolumeRootsDir: filepath.Join(root, "system-info"), VolumesDir: filepath.Join(root, "volumes")}
	if _, err := service.CheckpointWorkload(context.Background(), &ateompb.CheckpointWorkloadRequest{ActorDirs: dirs, Scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL}); err == nil || !strings.Contains(err.Error(), "Data snapshots only") {
		t.Fatalf("Full checkpoint did not fail before touching lifecycle: %v", err)
	}
	if _, err := service.RestoreWorkload(context.Background(), &ateompb.RestoreWorkloadRequest{ActorDirs: dirs, Scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL}); err == nil || !strings.Contains(err.Error(), "Data restores only") {
		t.Fatalf("Full restore did not fail before touching lifecycle: %v", err)
	}
}

func TestBlockRootfsKeyTracksConfigurationAndConfinesReads(t *testing.T) {
	bundle := blockRootfsTestBundle(t)
	initial, err := blockRootfsKey(bundle, 32)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := imagecache.ReadSpec(bundle)
	if err != nil {
		t.Fatal(err)
	}
	spec.ExtraDirs = []string{"/additional-mount"}
	if err := imagecache.WriteSpec(bundle, spec); err != nil {
		t.Fatal(err)
	}
	changed, err := blockRootfsKey(bundle, 32)
	if err != nil || changed == initial {
		t.Fatalf("mount configuration did not change key: %v", err)
	}
	resolver := filepath.Join(bundle, "rootfs", "etc", "resolv.conf")
	if err := os.WriteFile(resolver, []byte("nameserver 127.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dnsChanged, err := blockRootfsKey(bundle, 32)
	if err != nil || dnsChanged == changed {
		t.Fatalf("DNS configuration did not change key: %v", err)
	}
	if err := os.Remove(resolver); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", resolver); err != nil {
		t.Fatal(err)
	}
	if _, err := blockRootfsKey(bundle, 32); err == nil {
		t.Fatal("cache key read followed a host escape")
	}
}

func TestBlockRootfsLockWaitIsCancelable(t *testing.T) {
	bundle := blockRootfsTestBundle(t)
	cache := t.TempDir()
	key, err := blockRootfsKey(bundle, 32)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(cache, key+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ensureBlockRootfs(ctx, cache, bundle, "missing-mkfs", 32); err != context.Canceled {
		t.Fatalf("cache lock ignored cancellation: %v", err)
	}
}
