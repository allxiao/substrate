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

package kata

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/third_party/kata/agentpb"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/emptypb"
)

func BlockRootfsStorages(containerID, device string, upperMiB int) ([]*agentpb.Storage, string, error) {
	if containerID == "" || path.Base(containerID) != containerID || containerID == "." || containerID == ".." || strings.ContainsAny(containerID, ":,\x00") {
		return nil, "", fmt.Errorf("invalid block rootfs container ID %q", containerID)
	}
	index, indexErr := strconv.Atoi(strings.TrimPrefix(device, "/dev/pmem"))
	pmem := indexErr == nil && index >= 0 && index < 25 && device == fmt.Sprintf("/dev/pmem%d", index)
	if !pmem && (!strings.HasPrefix(device, "/dev/vd") || len(device) != len("/dev/vdb") || device[len(device)-1] < 'b' || device[len(device)-1] > 'z') {
		return nil, "", fmt.Errorf("invalid application block device %q", device)
	}
	if upperMiB <= 0 {
		return nil, "", fmt.Errorf("block rootfs upper limit must be positive")
	}
	base := path.Join("/run/kata-containers/block-rootfs", containerID)
	lower, writable, rootfs := path.Join(base, "lower"), path.Join(base, "writable"), path.Join(base, "rootfs")
	storages := []*agentpb.Storage{
		{Driver: "blk", Source: device, Fstype: "ext4", MountPoint: lower, Options: []string{"ro", "noload"}},
		{Driver: "ephemeral", Source: "tmpfs", Fstype: "tmpfs", MountPoint: writable, Options: []string{fmt.Sprintf("size=%dm", upperMiB), "mode=0755"}},
		{Driver: "overlayfs", Source: "overlay", Fstype: "overlay", MountPoint: rootfs, DriverOptions: []string{
			"io.katacontainers.volume.overlayfs.create_directory=" + path.Join(writable, "upper"),
			"io.katacontainers.volume.overlayfs.create_directory=" + path.Join(writable, "work"),
		}, Options: []string{
			"lowerdir=" + lower, "upperdir=" + path.Join(writable, "upper"), "workdir=" + path.Join(writable, "work"), "index=off",
		}},
	}
	if pmem {
		storages[0].Options = append(storages[0].Options, "dax=always")
	}
	return storages, rootfs, nil
}

func (a *AgentClient) StartBlockRootfsContainer(ctx context.Context, containerID string, spec *specs.Spec, device string, upperMiB int) error {
	storages, rootfs, err := BlockRootfsStorages(containerID, device, upperMiB)
	if err != nil {
		return err
	}
	pbSpec := SpecToAgentPB(spec)
	pbSpec.Root = &agentpb.Root{Path: rootfs}
	if pbSpec.Linux != nil {
		pbSpec.Linux.CgroupsPath = "/ateomchv/" + containerID
	}
	if err := a.CreateContainer(ctx, &agentpb.CreateContainerRequest{ContainerId: containerID, ExecId: containerID, OCI: pbSpec, Storages: storages}); err != nil {
		return fmt.Errorf("creating block rootfs container %q: %w", containerID, err)
	}
	upper := storages[1]
	remount := &agentpb.UpdateEphemeralMountsRequest{Storages: []*agentpb.Storage{{
		Driver: upper.Driver, Source: upper.Source, Fstype: upper.Fstype, MountPoint: upper.MountPoint,
		Options: append([]string{"remount"}, upper.Options...),
	}}}
	if err := a.client.Call(ctx, "grpc.AgentService", "UpdateEphemeralMounts", remount, &emptypb.Empty{}); err != nil {
		return fmt.Errorf("limiting block rootfs upper for %q: %w", containerID, err)
	}
	return a.StartContainer(ctx, containerID)
}
