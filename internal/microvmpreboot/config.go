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
	"fmt"
	"path/filepath"

	"github.com/agent-substrate/substrate/internal/nodepath"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/api/v1alpha1"
)

const EnvName = "ATE_MICROVM_PREBOOT"
const BackgroundCPUMilli int64 = 250

type Config struct {
	Spec   v1alpha1.MicroVMPrebootSpec  `json:"spec"`
	Assets map[string]map[string]string `json:"assets"`
}

func (config Config) Paths(arch string) (map[string]string, error) {
	if config.Spec.Count < 1 || config.Spec.Count > 4 || config.Spec.CPUMilli <= 0 || config.Spec.CPUMilli > 32000 || config.Spec.MemoryMiB < 256 || config.Spec.MemoryMiB > 32768 {
		return nil, fmt.Errorf("invalid preboot count or resource budget")
	}
	paths := make(map[string]string, 4)
	for _, name := range []string{"cloud-hypervisor", "virtiofsd", "kata-kernel", "kata-image"} {
		hash := config.Assets[arch][name]
		if err := resources.ValidateRunscHash(hash); err != nil {
			return nil, fmt.Errorf("preboot asset %s/%s: %w", arch, name, err)
		}
		paths[name] = filepath.Join(nodepath.StaticFilesDir, "runsc-"+hash)
	}
	return paths, nil
}

func (config Config) ReservedMemoryBytes() int64 {
	return int64(config.Spec.Count) * int64(config.Spec.MemoryMiB) * 1024 * 1024
}
