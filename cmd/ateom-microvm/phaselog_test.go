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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/ateattr"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/internal/resources"
)

func phaseLogAttribution() resources.ActorAttribution {
	return resources.ActorAttribution{
		Ref:              resources.ActorRef{Atespace: "team-a", Name: "support-agent-42"},
		UID:              "uid-abc",
		TemplateAtespace: "templates",
		TemplateName:     "support-agent",
	}
}

// renderPhaseRecord logs attrs through a local JSON handler, so the test sees
// the record a collector would parse rather than the slice that built it.
func renderPhaseRecord(t *testing.T, attrs []slog.Attr) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).LogAttrs(context.Background(), slog.LevelInfo, "Checkpoint timing breakdown", attrs...)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal record %q: %v", buf.String(), err)
	}
	return rec
}

func TestSnapshotPhaseAttrs(t *testing.T) {
	t.Parallel()

	attrs := snapshotPhaseAttrs(phaseLogAttribution(), ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
		checkpointDurationKey, nil, []phase{
			{phasePrep, 40 * time.Millisecond},
			{phasePause, 3 * time.Millisecond},
			{phaseSnapshot, 850 * time.Millisecond},
			// A capture that did not run stays off the record.
			{phaseDurableDir, 0},
			{phaseTeardown, 230 * time.Millisecond},
			{phaseTotal, 1250 * time.Millisecond},
		})

	// json.Unmarshal keeps the last of a repeated key, so a duplicate is
	// invisible in the rendered record and has to be caught on the slice.
	seen := make(map[string]bool, len(attrs))
	for _, attr := range attrs {
		if seen[attr.Key] {
			t.Errorf("key %s emitted twice", attr.Key)
		}
		seen[attr.Key] = true
	}

	rec := renderPhaseRecord(t, attrs)
	for k, want := range map[string]string{
		"ate.atespace":          "team-a",
		"ate.actor.name":        "support-agent-42",
		"ate.actor.uid":         "uid-abc",
		"ate.template.atespace": "templates",
		"ate.template.name":     "support-agent",
		"ate.snapshot.scope":    ateattr.SnapshotScopeFull,
	} {
		if got, ok := rec[k]; !ok {
			t.Errorf("missing %s", k)
		} else if got != want {
			t.Errorf("%s = %v, want %q", k, got, want)
		}
	}
	// Seconds, not slog.Duration's nanoseconds: the keys extend the atelet
	// histograms' names, and those declare unit s.
	for k, want := range map[string]float64{
		"ateom.actor.checkpoint.duration.prep":     0.04,
		"ateom.actor.checkpoint.duration.pause":    0.003,
		"ateom.actor.checkpoint.duration.snapshot": 0.85,
		"ateom.actor.checkpoint.duration.teardown": 0.23,
		"ateom.actor.checkpoint.duration.total":    1.25,
	} {
		got, ok := rec[k].(float64)
		if !ok {
			t.Errorf("%s = %v (%T), want a number", k, rec[k], rec[k])
		} else if got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	for _, k := range []string{
		"ateom.actor.checkpoint.duration.durable_dir",
		// The phase key names the one step a datapoint timed; this record
		// carries them all.
		"ate.snapshot.phase",
		// Absent error.type is success, as on the instruments.
		"error.type",
	} {
		if v, ok := rec[k]; ok {
			t.Errorf("%s is present with %v, want absent", k, v)
		}
	}
}

// TestSnapshotPhaseAttrsFailure: a checkpoint that died keeps the phases it
// completed and is marked, so a reader can exclude it from percentiles.
func TestSnapshotPhaseAttrsFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"a wrapped context deadline reports DeadlineExceeded",
			fmt.Errorf("while snapshotting guest: %w", context.DeadlineExceeded), "DeadlineExceeded"},
		{"a wrapped context cancellation reports Canceled",
			fmt.Errorf("while pausing guest: %w", context.Canceled), "Canceled"},
		{"a plain error is a bounded Unknown, not its message",
			fmt.Errorf("while clearing checkpoint dir: disk on fire"), "Unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := renderPhaseRecord(t, snapshotPhaseAttrs(phaseLogAttribution(),
				ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL, checkpointDurationKey, tt.err,
				[]phase{{phasePrep, 40 * time.Millisecond}, {phasePause, 3 * time.Millisecond}, {phaseTotal, 30 * time.Second}}))
			if got := rec["error.type"]; got != tt.want {
				t.Errorf("error.type = %v, want %q", got, tt.want)
			}
			// The phases that ran before the failure are still on the record;
			// the ones that never started are not.
			if _, ok := rec["ateom.actor.checkpoint.duration.pause"]; !ok {
				t.Error("missing ateom.actor.checkpoint.duration.pause")
			}
			if v, ok := rec["ateom.actor.checkpoint.duration.snapshot"]; ok {
				t.Errorf("snapshot phase present with %v, want absent", v)
			}
		})
	}
}

func TestRestoreDurablePhaseAttrs(t *testing.T) {
	t.Parallel()
	for _, scope := range []ateompb.SnapshotScope{
		ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
		ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA,
	} {
		t.Run(scope.String(), func(t *testing.T) {
			t.Parallel()
			record := renderPhaseRecord(t, snapshotPhaseAttrs(phaseLogAttribution(), scope,
				restoreDurationKey, nil, []phase{
					{phaseDurableDir, 12 * time.Millisecond},
					{phaseTotal, 3 * time.Second},
				}))
			if got := record[restoreDurationKey+"."+phaseDurableDir]; got != 0.012 {
				t.Errorf("durable_dir = %v, want 0.012 seconds", got)
			}
			if got := record[restoreDurationKey+"."+phaseTotal]; got != float64(3) {
				t.Errorf("total = %v, want 3 seconds", got)
			}
			if got := record[string(ateattr.SnapshotScopeKey)]; got != scopeLogValue(scope) {
				t.Errorf("scope = %v, want %s", got, scopeLogValue(scope))
			}
		})
	}
}

func TestStoragePreparationPhaseAttrs(t *testing.T) {
	t.Parallel()
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", failure), func(t *testing.T) {
			t.Parallel()
			var operationErr error
			if failure {
				operationErr = context.DeadlineExceeded
			}
			record := renderPhaseRecord(t, snapshotPhaseAttrs(phaseLogAttribution(),
				ateompb.SnapshotScope_SNAPSHOT_SCOPE_UNSPECIFIED, storageDurationKey, operationErr, []phase{
					{phaseDurableBind, 2 * time.Millisecond},
					{phaseVirtiofsd, 5 * time.Millisecond},
					{phaseTotal, 20 * time.Millisecond},
				}))
			for name, want := range map[string]float64{
				phaseDurableBind: 0.002,
				phaseVirtiofsd:   0.005,
				phaseTotal:       0.020,
			} {
				if got := record[storageDurationKey+"."+name]; got != want {
					t.Errorf("%s = %v, want %v seconds", name, got, want)
				}
			}
			if got := record["ate.actor.uid"]; got != phaseLogAttribution().UID {
				t.Errorf("actor UID = %v", got)
			}
			if failure && record["error.type"] != "DeadlineExceeded" {
				t.Errorf("error.type = %v, want DeadlineExceeded", record["error.type"])
			}
			if !failure {
				if _, exists := record["error.type"]; exists {
					t.Error("successful storage preparation reports an error")
				}
			}
		})
	}
}

func TestStoragePreparationReportsFailedDurableBind(t *testing.T) {
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previousLogger)

	service := &AteomService{}
	command, err := service.stageMergedRootfs(t.Context(), resolvedRuntime{}, phaseLogAttribution(),
		&ateompb.ActorDirs{
			OciBundleDir:              t.TempDir(),
			DurableDirVolumeMountsDir: filepath.Join(t.TempDir(), "missing"),
		}, nil, []*ateompb.Container{{
			DurableDirVolumeMounts: []*ateompb.DurableDirVolumeMount{{VolumeName: "home"}},
		}}, nil)
	if err == nil || command != nil {
		t.Fatalf("storage preparation = %v, %v, want failed bind", command, err)
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode storage timing log: %v", err)
	}
	for _, name := range []string{phaseDurableBind, phaseTotal} {
		if elapsed, ok := record[storageDurationKey+"."+name].(float64); !ok || elapsed <= 0 {
			t.Errorf("%s = %v, want positive seconds", name, record[storageDurationKey+"."+name])
		}
	}
	if _, exists := record[storageDurationKey+"."+phaseVirtiofsd]; exists {
		t.Error("virtiofsd phase recorded before it ran")
	}
	if record["error.type"] != "Unknown" || record["ate.actor.uid"] != phaseLogAttribution().UID {
		t.Errorf("failed storage preparation lost error or actor context: %v", record)
	}
}

// TestScopeLogValue pins the mapping onto the shared scope values, so the
// ateom and atelet records of one operation agree on the scope they carry.
func TestScopeLogValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		scope ateompb.SnapshotScope
		want  string
	}{
		{ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL, ateattr.SnapshotScopeFull},
		{ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA, ateattr.SnapshotScopeData},
		{ateompb.SnapshotScope_SNAPSHOT_SCOPE_UNSPECIFIED, ateattr.SnapshotScopeUnknown},
		{ateompb.SnapshotScope(99), ateattr.SnapshotScopeUnknown},
	}
	for _, tt := range tests {
		if got := scopeLogValue(tt.scope); got != tt.want {
			t.Errorf("scopeLogValue(%v) = %q, want %q", tt.scope, got, tt.want)
		}
	}
}
