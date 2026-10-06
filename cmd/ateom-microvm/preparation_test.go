//go:build linux

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
	"testing"

	"golang.org/x/sync/errgroup"
)

func TestColdPreparationsOverlapAndJoin(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	prepare := func(ctx context.Context) error {
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	group := new(errgroup.Group)
	group.Go(func() error { return runColdPreparations(t.Context(), prepare, prepare, prepare) })
	for range 3 {
		select {
		case <-started:
		case <-t.Context().Done():
			t.Fatal("preparations did not overlap")
		}
	}
	close(release)
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestColdPreparationsCancelOnFailure(t *testing.T) {
	err := runColdPreparations(t.Context(), func(context.Context) error { return fmt.Errorf("restore failed") }, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	if err == nil {
		t.Fatal("preparation failure did not block user startup")
	}
}
