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

package microvmpreboot

import (
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/api/v1alpha1"
)

func TestConfigUsesOnlyVerifiedCacheNames(t *testing.T) {
	config := Config{Spec: v1alpha1.MicroVMPrebootSpec{Count: 1, CPUMilli: 1000, MemoryMiB: 512}, Assets: map[string]map[string]string{"amd64": {}}}
	for _, name := range []string{"cloud-hypervisor", "virtiofsd", "kata-kernel", "kata-image"} {
		config.Assets["amd64"][name] = strings.Repeat("a", 64)
	}
	paths, err := config.Paths("amd64")
	if err != nil || len(paths) != 4 {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	if config.ReservedMemoryBytes() != 512*1024*1024 {
		t.Fatal("wrong idle memory reservation")
	}
	config.Assets["amd64"]["kata-image"] = "../escape"
	if _, err := config.Paths("amd64"); err == nil {
		t.Fatal("unsafe content hash accepted")
	}
	if _, err := config.Paths("arm64"); err == nil {
		t.Fatal("missing architecture accepted")
	}
}
