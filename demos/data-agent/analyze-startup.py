# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import json
import re
import statistics
import sys


def analyze(text):
    markers = []
    imports = []
    for line in text.splitlines():
        if line.startswith("AGENT_STARTUP "):
            markers.append(json.loads(line.removeprefix("AGENT_STARTUP ")))
        match = re.match(r"import time:\s*(\d+)\s*\|\s*(\d+)\s*\|\s*(.*)", line)
        if match:
            imports.append({"module": match[3].strip(), "self_ms": int(match[1]) / 1000, "cumulative_ms": int(match[2]) / 1000})
    return {
        "markers": markers,
        "import_count": len(imports),
        "total_import_self_ms": sum(row["self_ms"] for row in imports),
        "largest_self": sorted(imports, key=lambda row: row["self_ms"], reverse=True)[:20],
        "largest_subtrees": sorted(imports, key=lambda row: row["cumulative_ms"], reverse=True)[:15],
    }


def main():
    if len(sys.argv) == 1:
        print(json.dumps(analyze(sys.stdin.read()), indent=2))
        return
    samples = []
    for filename in sys.argv[1:]:
        with open(filename, encoding="utf-8") as source:
            samples.append(analyze(source.read()))
    stages = sorted({marker["stage"] for sample in samples for marker in sample["markers"]})
    summary = []
    for stage in stages:
        markers = [marker for sample in samples for marker in sample["markers"] if marker["stage"] == stage]
        summary.append({
            "stage": stage,
            "samples": len(markers),
            "wall_ms_p50": statistics.median(marker["wall_ms"] for marker in markers),
            "cpu_ms_p50": statistics.median(marker["cpu_ms"] for marker in markers),
            "wall_ms_min": min(marker["wall_ms"] for marker in markers),
            "wall_ms_max": max(marker["wall_ms"] for marker in markers),
        })
    print(json.dumps({"samples": len(samples), "phases": summary}, indent=2))


if __name__ == "__main__":
    main()
