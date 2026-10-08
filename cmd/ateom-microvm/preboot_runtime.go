// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/ch"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
	"github.com/agent-substrate/substrate/internal/ateomcgroup"
	"github.com/agent-substrate/substrate/internal/ateomnet"
	"github.com/agent-substrate/substrate/internal/ateomtunnel"
	"github.com/agent-substrate/substrate/internal/microvmpreboot"
	"github.com/agent-substrate/substrate/internal/wakeupprobe"
	"github.com/google/uuid"
)

func (s *AteomService) startPreboot(ctx context.Context) (*microvmpreboot.Config, error) {
	value := os.Getenv(microvmpreboot.EnvName)
	if value == "" {
		return nil, nil
	}
	var config microvmpreboot.Config
	if err := json.Unmarshal([]byte(value), &config); err != nil {
		return nil, fmt.Errorf("preboot config: %w", err)
	}
	paths, err := config.Paths(runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	if !s.actorCgroups {
		return nil, fmt.Errorf("preboot requires delegated actor cgroups")
	}
	_, _, params := s.guestConfig()
	memory, err := resolveGuestMemMiB(int64(config.Spec.MemoryMiB)*1024*1024, s.memReserveMiB, 0)
	if err != nil {
		return nil, err
	}
	key := prebootKey{kernel: paths[assetKernel], image: paths[assetImage], vmm: paths[assetCH], virtiofsd: paths[assetVirtiofsd], memoryMiB: memory, vcpus: int((config.Spec.CPUMilli + 999) / 1000)}
	s.preboot = newPrebootPool(ctx, key, int(config.Spec.Count), func(ctx context.Context) (*prebootVM, error) {
		vm, err := s.bootPreboot(ctx, key, params)
		if err != nil {
			slog.WarnContext(ctx, "Preboot VM preparation failed", slog.Any("err", err))
		}
		return vm, err
	}, s.disposePreboot, func(ctx context.Context, vm *prebootVM) bool {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return ch.NewClient(vm.runtime.apiSocket).Running(ctx)
	})
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.preboot.mu.Lock()
		ready := len(s.preboot.ready) == int(config.Spec.Count)
		s.preboot.mu.Unlock()
		if ready {
			return &config, nil
		}
		select {
		case <-ctx.Done():
			s.preboot.close()
			return nil, ctx.Err()
		case <-deadline.C:
			s.preboot.close()
			return nil, fmt.Errorf("preboot startup timed out waiting for assets and ready VMs")
		case <-ticker.C:
		}
	}
}

func (s *AteomService) bootPreboot(ctx context.Context, key prebootKey, params string) (_ *prebootVM, retErr error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for _, path := range []string{key.kernel, key.image, key.vmm, key.virtiofsd} {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("preboot asset is not a regular file: %s", path)
		}
	}
	vm := &prebootVM{id: uuid.NewString(), runtime: &runningActor{}}
	vm.runtime.baseID = vm.id
	vm.runtime.runtimeID = vm.id
	defer func() {
		if retErr != nil {
			s.disposePreboot(vm)
		}
	}()
	if err := os.MkdirAll(kata.VMDir(vm.id), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(kata.SharedDir(vm.id), 0o700); err != nil {
		return nil, err
	}
	leaf, err := ateomcgroup.OpenActorLeaf(vm.id, microvmpreboot.BackgroundCPUMilli)
	if err != nil {
		return nil, err
	}
	defer leaf.Close()
	vm.runtime.vfsdCmd, err = kata.StartVirtiofsd(ctx, kata.VirtiofsdOptions{Binary: key.virtiofsd, SocketPath: kata.VirtiofsdSocketPath(vm.id), SharedDir: kata.SharedDir(vm.id), SysProcAttr: leaf.SysProcAttr(), Log: slogWriter{ctx}})
	if err != nil {
		return nil, err
	}
	vm.runtime.apiSocket = filepath.Join(kata.VMDir(vm.id), "clh-api.sock")
	cmd, client, err := ch.LaunchVMM(ctx, ch.LaunchVMMOptions{Binary: key.vmm, APISocket: vm.runtime.apiSocket, SysProcAttr: leaf.SysProcAttr(), Stdout: slogWriter{ctx}, Stderr: slogWriter{ctx}})
	vm.runtime.chCmd = cmd
	if err != nil {
		return nil, err
	}
	config := buildVMConfig(vm.id, key.kernel, key.image, params, kata.ConsoleLogPath(vm.id), key.memoryMiB, key.vcpus, agentInit(ctx, client.Info()), s.kataDebug)
	if s.memoryTHP {
		config.Memory, err = prepareTHPMemory(filepath.Join(kata.VMDir(vm.id), "memory"), key.memoryMiB)
		if err != nil {
			return nil, err
		}
	}
	if err := client.CreateVM(ctx, config); err != nil {
		return nil, err
	}
	if err := client.BootVM(ctx); err != nil {
		return nil, err
	}
	if !waitForFile(kata.VsockSocketPath(vm.id), 15*time.Second) {
		return nil, fmt.Errorf("preboot vsock missing")
	}
	vm.runtime.guestAgent, err = dialAgentRetry(ctx, kata.VsockSocketPath(vm.id), 60*time.Second)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "Preboot VM ready", slog.String("id", vm.id), slog.Int("guest_memory_mib", key.memoryMiB), slog.Int("vcpus", key.vcpus))
	return vm, nil
}

func (s *AteomService) disposePreboot(vm *prebootVM) {
	if vm == nil {
		return
	}
	if vm.runtime != nil {
		if vm.runtime.guestAgent != nil {
			_ = vm.runtime.guestAgent.Close()
		}
		for _, cmd := range []*exec.Cmd{vm.runtime.chCmd, vm.runtime.vfsdCmd} {
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.cleanupSandboxState(ctx, vm.id)
	_ = ateomcgroup.RemoveActorLeaf(vm.id)
}

func (s *AteomService) bindPreboot(ctx context.Context, p actorBootParams, ctrs []actorContainer, egress *ateomtunnel.ActorEgress, vm *prebootVM) (retErr error) {
	defer func() {
		if retErr != nil {
			s.disposePreboot(vm)
		}
		s.preboot.completeClaim()
	}()
	s.cleanupSandboxState(ctx, p.actorUID)
	if err := resetRootfsUpperDir(p.actorDirs); err != nil {
		return err
	}
	leaf, err := s.actorLeaf(p.actorUID, p.size)
	if err != nil {
		return err
	}
	defer leaf.Close()
	if err := leaf.Adopt(vm.id); err != nil {
		return err
	}
	if err := ateomcgroup.RemoveActorLeaf(vm.id); err != nil {
		return err
	}
	if _, err := s.stageActorShares(ctx, vm.id, p.actorDirs, ctrs, p.containers); err != nil {
		return err
	}
	client := ch.NewClient(vm.runtime.apiSocket)
	for _, container := range ctrs {
		if container.blockImage == "" {
			continue
		}
		if s.blockAccess == "virtio-pmem" {
			err = client.AddPmem(ctx, ch.PmemConfig{File: container.blockImage, Size: int64(s.blockImageMiB) * 1024 * 1024, DiscardWrites: true})
		} else {
			err = client.AddDisk(ctx, ch.DiskConfig{Path: container.blockImage, Readonly: true, ImageType: "Raw", NumQueues: 1, QueueSize: 1024})
		}
		if err != nil {
			return fmt.Errorf("attaching preboot application disk: %w", err)
		}
		if err := waitPrebootDevice(ctx, kata.VsockSocketPath(vm.id), container.blockDevice); err != nil {
			return err
		}
	}
	tap, err := setupActorTap(ctx, s.sandboxNetNS(p.actorUID), "tap0_kata", 1)
	if err != nil {
		return err
	}
	defer func() {
		for _, file := range tap {
			_ = file.Close()
		}
	}()
	var fds []int
	for _, file := range tap {
		fds = append(fds, int(file.Fd()))
	}
	if err := client.AddNetWithFDs(ctx, actorGuestMAC, 2*len(fds), fds); err != nil {
		return err
	}
	if err := waitPrebootCondition(ctx, kata.VsockSocketPath(vm.id), "for interface in /sys/class/net/*; do test \"$(cat \"$interface/address\")\" = '"+actorGuestMAC+"' && printf '__PREBOOT''_READY__\\n'; done", "Actor network device"); err != nil {
		return err
	}
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return err
	}
	if err := vm.runtime.guestAgent.ReseedRandomDev(ctx, entropy); err != nil {
		return err
	}
	if err := s.startActorContainers(ctx, vm.runtime.guestAgent, p.actorUID, kata.VsockSocketPath(vm.id), ctrs); err != nil {
		return err
	}
	if err := wakeupprobe.WaitAll(ctx, p.containers, ateomnet.ActorVethIP, wakeupprobe.DialFunc(s.sandboxDialer(p.actorUID))); err != nil {
		return err
	}
	if err := s.tunnel.Activate(p.attribution(), s.sandboxDialer(p.actorUID), egress); err != nil {
		return err
	}
	vm.runtime.workloadIDs = workloadIDs(ctrs)
	s.setRunningVM(p.actorUID, vm.runtime)
	for _, container := range ctrs {
		s.startActorLogForwarding(vm.runtime.guestAgent, p.actorAttribution(), container.name, container.name)
	}
	s.setGuestStats(p.actorUID, &guestStatsTarget{actorUID: p.actorUID, agent: vm.runtime.guestAgent, workloadIDs: workloadIDs(ctrs)})
	slog.InfoContext(ctx, "Preboot VM claimed", slog.String("id", vm.id), slog.String("actor_uid", p.actorUID))
	return nil
}

func waitPrebootDevice(ctx context.Context, vsock, device string) error {
	return waitPrebootCondition(ctx, vsock, "test -b "+device+" && printf '__PREBOOT''_READY__\\n'", device)
}

func waitPrebootCondition(ctx context.Context, vsock, command, description string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		output := kata.DebugConsoleDump(ctx, vsock, command)
		if strings.Contains(output, "__PREBOOT_READY__") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("preboot device %s did not appear", description)
		case <-ticker.C:
		}
	}
}
