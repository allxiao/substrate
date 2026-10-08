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
	"sync"
	"time"
)

type prebootKey struct {
	kernel, image, vmm, virtiofsd string
	memoryMiB, vcpus              int
}

type prebootVM struct {
	id      string
	runtime *runningActor
}

type prebootPool struct {
	mu      sync.Mutex
	key     prebootKey
	target  int
	ready   []*prebootVM
	pending int
	closed  bool
	cancel  context.CancelFunc
	wake    chan struct{}
	done    chan struct{}
	boot    func(context.Context) (*prebootVM, error)
	dispose func(*prebootVM)
	healthy func(context.Context, *prebootVM) bool
}

func newPrebootPool(ctx context.Context, key prebootKey, target int, boot func(context.Context) (*prebootVM, error), dispose func(*prebootVM), healthy func(context.Context, *prebootVM) bool) *prebootPool {
	ctx, cancel := context.WithCancel(ctx)
	pool := &prebootPool{key: key, target: target, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), boot: boot, dispose: dispose, healthy: healthy}
	go pool.maintain(ctx)
	return pool
}

func (pool *prebootPool) take(key prebootKey) *prebootVM {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed || key != pool.key || len(pool.ready) == 0 {
		return nil
	}
	last := len(pool.ready) - 1
	vm := pool.ready[last]
	pool.ready = pool.ready[:last]
	pool.pending++
	return vm
}

func (pool *prebootPool) completeClaim() {
	pool.mu.Lock()
	if pool.pending > 0 {
		pool.pending--
	}
	pool.mu.Unlock()
	select {
	case pool.wake <- struct{}{}:
	default:
	}
}

func (pool *prebootPool) close() {
	pool.mu.Lock()
	pool.closed = true
	pool.mu.Unlock()
	pool.cancel()
	<-pool.done
}

func (pool *prebootPool) maintain(ctx context.Context) {
	healthTick := time.NewTicker(time.Second)
	defer healthTick.Stop()
	defer close(pool.done)
	defer func() {
		pool.mu.Lock()
		pool.closed = true
		ready := pool.ready
		pool.ready = nil
		pool.mu.Unlock()
		for _, vm := range ready {
			pool.dispose(vm)
		}
	}()
	for ctx.Err() == nil {
		pool.mu.Lock()
		full := len(pool.ready)+pool.pending >= pool.target
		pool.mu.Unlock()
		if full {
			select {
			case <-ctx.Done():
				return
			case <-pool.wake:
			case <-healthTick.C:
				pool.checkHealth(ctx)
			}
			continue
		}
		vm, err := pool.boot(ctx)
		if err != nil {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		if ctx.Err() != nil {
			pool.dispose(vm)
			return
		}
		pool.mu.Lock()
		if pool.closed {
			pool.mu.Unlock()
			pool.dispose(vm)
			return
		}
		pool.ready = append(pool.ready, vm)
		pool.mu.Unlock()
	}
}

func (pool *prebootPool) checkHealth(ctx context.Context) {
	if pool.healthy == nil {
		return
	}
	pool.mu.Lock()
	ready := append([]*prebootVM(nil), pool.ready...)
	pool.mu.Unlock()
	for _, vm := range ready {
		if pool.healthy(ctx, vm) {
			continue
		}
		removed := false
		pool.mu.Lock()
		for index, item := range pool.ready {
			if item == vm {
				pool.ready = append(pool.ready[:index], pool.ready[index+1:]...)
				removed = true
				break
			}
		}
		pool.mu.Unlock()
		if removed {
			pool.dispose(vm)
		}
	}
}
