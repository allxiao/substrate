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
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestBlockRootfsStorages(t *testing.T) {
	storages, rootfs, err := BlockRootfsStorages("agent", "/dev/vdb", 64)
	if err != nil {
		t.Fatal(err)
	}
	if rootfs != "/run/kata-containers/block-rootfs/agent/rootfs" || len(storages) != 3 {
		t.Fatalf("unexpected rootfs %q and storages %+v", rootfs, storages)
	}
	if storages[0].Driver != "blk" || storages[0].Source != "/dev/vdb" || !reflect.DeepEqual(storages[0].Options, []string{"ro", "noload"}) {
		t.Fatalf("lower is not a read-only application disk: %+v", storages[0])
	}
	if storages[1].Driver != "ephemeral" || !reflect.DeepEqual(storages[1].Options, []string{"size=64m", "mode=0755"}) {
		t.Fatalf("upper is not bounded guest tmpfs: %+v", storages[1])
	}
	if storages[2].Driver != "overlayfs" || !strings.Contains(strings.Join(storages[2].Options, ","), "upperdir=/run/kata-containers/block-rootfs/agent/writable/upper") {
		t.Fatalf("unexpected overlay: %+v", storages[2])
	}
}

func TestBlockRootfsStoragesRejectsUnsafeInputs(t *testing.T) {
	for _, containerID := range []string{"", ".", "..", "../escape", "agent,bad", "agent:bad"} {
		if _, _, err := BlockRootfsStorages(containerID, "/dev/vdb", 64); err == nil {
			t.Errorf("accepted container ID %q", containerID)
		}
	}
	for _, device := range []string{"/dev/vda", "/dev/vdb1", "/dev/sda", "/dev/vd/"} {
		if _, _, err := BlockRootfsStorages("agent", device, 64); err == nil {
			t.Errorf("accepted application disk %q", device)
		}
	}
	if _, _, err := BlockRootfsStorages("agent", "/dev/vdb", 0); err == nil {
		t.Fatal("accepted unbounded upper")
	}
}

func TestBlockRootfsPmemIsReadOnlyDAX(t *testing.T) {
	for index := range 25 {
		storages, _, err := BlockRootfsStorages("agent", fmt.Sprintf("/dev/pmem%d", index), 64)
		if err != nil {
			t.Fatal(err)
		}
		if storages[0].Driver != "blk" || !reflect.DeepEqual(storages[0].Options, []string{"ro", "noload", "dax=always"}) {
			t.Fatalf("unsafe pmem storage: %+v", storages[0])
		}
	}
	for _, device := range []string{"/dev/pmem25", "/dev/pmem-1", "/dev/pmem01", "/dev/pmem0/../1", "/dev/pmem"} {
		if _, _, err := BlockRootfsStorages("agent", device, 64); err == nil {
			t.Fatalf("noncanonical pmem path accepted: %s", device)
		}
	}
}
