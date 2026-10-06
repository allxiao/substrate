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

package controlapi

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func TestImagePreloadDeduplicatesNodesAndRecordsFailure(t *testing.T) {
	nodes := []string{"node-a", "node-a", "node-b"}
	for _, failure := range []bool{false, true} {
		state, err := preloadTemplateNodes(t.Context(), nodes, func(_ context.Context, node string) error {
			if failure && node == "node-b" {
				return fmt.Errorf("registry unavailable")
			}
			return nil
		})
		if len(state.DesiredNodes) != 2 || state.Ready == failure || (err != nil) != failure {
			t.Fatalf("unexpected preload state: %v, %v", state, err)
		}
		if !failure && len(state.CachedNodes) != 2 {
			t.Fatal("nodes not fully warmed")
		}
	}
}

func TestPreloadIncludesNodesWithoutWorkers(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nil)
	for index, node := range []string{"node-a", "node-b", "node-a", ""} {
		if err := indexer.Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("atelet-%d", index)}, Spec: corev1.PodSpec{NodeName: node}}); err != nil {
			t.Fatal(err)
		}
	}
	dialer := NewAteletDialer(indexer, "", "", "")
	nodes := dialer.NodeNames()
	if len(nodes) != 2 || nodes[0] != "node-a" || nodes[1] != "node-b" {
		t.Fatalf("preload nodes = %v", nodes)
	}
}
