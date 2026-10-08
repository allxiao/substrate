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
	"sync/atomic"
	"testing"
	"time"
)

func awaitPreboot(t *testing.T, pool *prebootPool) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		pool.mu.Lock()
		ready := len(pool.ready) > 0
		pool.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("preboot did not finish")
		case <-tick.C:
		}
	}
}

func TestPrebootPoolSingleUseMatchingAndCleanup(t *testing.T) {
	key := prebootKey{memoryMiB: 384, vcpus: 1, kernel: "kernel"}
	var boots, disposed atomic.Int32
	pool := newPrebootPool(t.Context(), key, 1, func(context.Context) (*prebootVM, error) { boots.Add(1); return &prebootVM{}, nil }, func(*prebootVM) { disposed.Add(1) }, nil)
	awaitPreboot(t, pool)
	if pool.take(prebootKey{memoryMiB: 512, vcpus: 1}) != nil {
		t.Fatal("mismatched VM claimed")
	}
	claimed := pool.take(key)
	if claimed == nil {
		t.Fatal("matching VM not claimed")
	}
	pool.mu.Lock()
	if len(pool.ready) != 0 || pool.pending != 1 || boots.Load() != 1 {
		t.Fatal("refilled during Actor initialization")
	}
	pool.mu.Unlock()
	pool.completeClaim()
	awaitPreboot(t, pool)
	pool.close()
	if pool.take(key) != nil {
		t.Fatal("closed pool returned VM")
	}
	if boots.Load() != 2 || disposed.Load() != 1 {
		t.Fatalf("boots=%d disposed=%d", boots.Load(), disposed.Load())
	}
}

func TestPrebootPoolClosesInFlightBuild(t *testing.T) {
	started := make(chan struct{})
	var disposed atomic.Int32
	pool := newPrebootPool(t.Context(), prebootKey{}, 1, func(ctx context.Context) (*prebootVM, error) { close(started); <-ctx.Done(); return &prebootVM{}, nil }, func(*prebootVM) { disposed.Add(1) }, nil)
	<-started
	pool.close()
	if disposed.Load() != 1 {
		t.Fatal("cancelled build leaked VM")
	}
}

func TestPrebootPoolDiscardsDeadReadyVM(t *testing.T) {
	var disposed atomic.Int32
	pool := newPrebootPool(t.Context(), prebootKey{}, 1, func(context.Context) (*prebootVM, error) { return &prebootVM{}, nil }, func(*prebootVM) { disposed.Add(1) }, func(context.Context, *prebootVM) bool { return false })
	awaitPreboot(t, pool)
	pool.checkHealth(t.Context())
	if disposed.Load() != 1 {
		t.Fatal("dead idle VM retained")
	}
	if pool.take(prebootKey{}) != nil {
		t.Fatal("dead VM claimed")
	}
	pool.close()
}
