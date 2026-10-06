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
	"sort"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"golang.org/x/sync/errgroup"
)

type templateImagePreloader interface {
	PreloadActorTemplate(context.Context, *ateapipb.ActorTemplate) (*ateapipb.ImagePreloadStatus, error)
}

func (service *RPCService) PreloadActorTemplate(ctx context.Context, template *ateapipb.ActorTemplate) (*ateapipb.ImagePreloadStatus, error) {
	return preloadTemplateNodes(ctx, service.dialer.NodeNames(), func(ctx context.Context, node string) error {
		connection, err := service.dialer.DialForAteletOnNode(node)
		if err != nil {
			return err
		}
		_, err = ateletpb.NewAteomHerderClient(connection).PreloadImage(ctx, &ateletpb.PreloadImageRequest{
			Image: template.Containers[0].Image, PinOwner: template.GetMetadata().GetUid(), PinTtlSeconds: 3600,
		})
		return err
	})
}

func preloadTemplateNodes(ctx context.Context, nodeNames []string, preload func(context.Context, string) error) (*ateapipb.ImagePreloadStatus, error) {
	state := &ateapipb.ImagePreloadStatus{}
	nodes := map[string]bool{}
	for _, node := range nodeNames {
		if node != "" {
			nodes[node] = true
		}
	}
	for node := range nodes {
		state.DesiredNodes = append(state.DesiredNodes, node)
	}
	sort.Strings(state.DesiredNodes)
	if len(nodes) == 0 {
		state.ErrorMessage = "no atelet nodes available for image preload"
		return state, fmt.Errorf("%s", state.ErrorMessage)
	}
	var mutex sync.Mutex
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for _, node := range state.DesiredNodes {
		group.Go(func() error {
			attempt, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			if err := preload(attempt, node); err != nil {
				return fmt.Errorf("preloading node %s: %w", node, err)
			}
			mutex.Lock()
			state.CachedNodes = append(state.CachedNodes, node)
			mutex.Unlock()
			return nil
		})
	}
	err := group.Wait()
	sort.Strings(state.CachedNodes)
	state.Ready = err == nil
	if err != nil {
		state.ErrorMessage = truncateUTF8(err.Error(), 4096)
	}
	return state, err
}
