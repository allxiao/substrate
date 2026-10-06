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
import importlib.util
import os
import py_compile
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from app import HistoryClient, create_app


def test_startup_profile_is_optional_and_ready_is_marked_once(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr("app._startup_enabled", True)
    with TestClient(create_app(tmp_path, str(uuid.uuid4()))) as client:
        assert client.get("/readyz").status_code == 200
        assert client.get("/readyz").status_code == 200
    records = [json.loads(line.removeprefix("AGENT_STARTUP ")) for line in capsys.readouterr().err.splitlines() if line.startswith("AGENT_STARTUP ")]
    assert sum(record["stage"] == "first_ready_request" for record in records) == 1
    assert all(record["wall_ms"] >= 0 and record["cpu_ms"] >= 0 for record in records)
    monkeypatch.setattr("app._startup_enabled", False)
    create_app(tmp_path, str(uuid.uuid4()))
    assert "AGENT_STARTUP " not in capsys.readouterr().err


def test_startup_profile_captures_entrypoint_boundary(monkeypatch, capsys):
    from app import startup_marker

    monkeypatch.setattr("app._startup_enabled", True)
    origin = time.time_ns() - 1_000_000
    monkeypatch.setenv("AGENT_ENTRYPOINT_START_NS", str(origin))
    startup_marker("boundary_test")
    record = json.loads(capsys.readouterr().err.removeprefix("AGENT_STARTUP "))
    assert record["since_entrypoint_ms"] >= 1
    assert record["since_entrypoint_ms"] == (record["time_ns"] - origin) / 1_000_000


def test_import_profile_uses_exclusive_time_without_double_counting():
    spec = importlib.util.spec_from_file_location("startup_analysis", Path(__file__).with_name("analyze-startup.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    report = module.analyze('import time: 1000 | 1000 | child\nimport time: 200 | 1200 | parent\nAGENT_STARTUP {"stage":"first_ready_request","wall_ms":1,"cpu_ms":0.5}\n')
    assert report["total_import_self_ms"] == 1.2
    assert report["largest_subtrees"][0]["module"] == "parent"
    assert report["markers"][0]["stage"] == "first_ready_request"


def test_request_profile_preserves_response_and_marks_history(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr("app._startup_enabled", True)
    with TestClient(create_app(tmp_path, str(uuid.uuid4()))) as client:
        response = client.post("/v1/responses", json={"input": "profiled"})
        assert response.status_code == 200
    records = [json.loads(line.removeprefix("AGENT_REQUEST ")) for line in capsys.readouterr().err.splitlines() if line.startswith("AGENT_REQUEST ")]
    assert [record["stage"] for record in records] == ["request_handler_enter", "responses_parse_validate", "history_thread_enter", "home_mkdir_open", "history_flock", "history_read", "history_append_flush", "history_fsync", "maf_result_return", "responses_encode"]
    assert all(record["wall_ms"] >= 0 for record in records)
    assert [json.loads(line) for line in (tmp_path / "history.txt").read_text().splitlines()] == ["profiled"]


def test_loading_file_benchmark_checks_identical_bytes(tmp_path):
    spec = importlib.util.spec_from_file_location("loading_ab", Path(__file__).with_name("loading-ab.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    paths = [tmp_path / "one", tmp_path / "two"]
    for path, content in zip(paths, (b"first", b"second")):
        path.write_bytes(content)
    for operation in ("stat", "read"):
        result = module.measure_files(paths, operation)
        assert result["files"] == 2
        assert result["bytes"] == 11
        assert result["wall_ms"] >= 0
        assert result["user_ms"] >= 0 and result["system_ms"] >= 0
    assert module.measure_files(paths, "open_close")["files"] == 2
    assert [result["bytes"] for result in module.measure_held_reads(paths)] == [11, 11, 11]
    assert [result["bytes"] for result in module.measure_held_reads(paths * 65)] == [715, 715, 715]
    with pytest.raises(ValueError):
        module.measure_files(paths, "invalid")


def test_loading_ram_copy_preserves_bytecode_and_native_fallback(tmp_path):
    spec = importlib.util.spec_from_file_location("loading_ab", Path(__file__).with_name("loading-ab.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    source = tmp_path / "source"
    package = source / "pure"
    package.mkdir(parents=True)
    bytecode = package / "cache.pyc"
    bytecode.write_bytes(b"cached-bytecode")
    native = source / "native"
    native.mkdir()
    (native / "extension.so").write_bytes(b"native-library")
    destination = tmp_path / "ram"
    result = module.prepare_ram_site(source, destination)
    copied = destination / "pure" / "cache.pyc"
    assert copied.read_bytes() == bytecode.read_bytes()
    assert copied.stat().st_mtime_ns == bytecode.stat().st_mtime_ns
    assert result["excluded"] == ["native"]
    assert not (destination / "native").exists()


def test_loading_bytecode_probe_repairs_only_source_mtime(tmp_path):
    spec = importlib.util.spec_from_file_location("loading_ab", Path(__file__).with_name("loading-ab.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    source = tmp_path / "example.py"
    content = b"value = 1\n"
    source.write_bytes(content)
    cache = Path(py_compile.compile(str(source), doraise=True))
    bytecode = cache.read_bytes()
    os.utime(source, (source.stat().st_atime, source.stat().st_mtime + 10))
    assert module.inspect_bytecode(tmp_path)["mtime_mismatch"] == 1
    assert module.inspect_bytecode(tmp_path, repair=True)["repaired"] == 1
    assert module.inspect_bytecode(tmp_path)["valid_timestamp"] == 1
    assert source.read_bytes() == content
    assert cache.read_bytes() == bytecode


def output_text(response):
    return "".join(content["text"] for item in response["output"] for content in item.get("content", []) if content["type"] == "output_text")


def test_history_excludes_current_and_survives_restart(tmp_path):
    session_id = str(uuid.uuid4())
    with TestClient(create_app(tmp_path, session_id)) as client:
        first_boot = client.get("/readyz").json()["boot_id"]
        for query_number in range(1, 8):
            response = client.post("/v1/responses", json={"input": f"query-{query_number}"})
            assert response.status_code == 200, response.text
        expected = f"You asked: query-7\nCurrent Session: {session_id}\n\nYou previously asked:\n" + "\n".join(f"query-{number}" for number in range(2, 7))
        assert output_text(response.json()) == expected
    with TestClient(create_app(tmp_path, session_id)) as restarted:
        assert restarted.get("/readyz").json()["boot_id"] != first_boot
        response = restarted.post("/v1/responses", json={"input": "query-8"})
        assert f"Current Session: {session_id}" in output_text(response.json())
        assert output_text(response.json()).endswith("\n".join(f"query-{number}" for number in range(3, 8)))


def test_stream_appends_once(tmp_path):
    session_id = str(uuid.uuid4())
    with TestClient(create_app(tmp_path, session_id)) as client:
        response = client.post("/v1/responses", json={"input": "streamed", "stream": True})
        assert response.status_code == 200, response.text
        assert "response.completed" in response.text
        assert "Current Session:" in response.text
    assert [json.loads(record) for record in (tmp_path / "history.txt").read_text().splitlines()] == ["streamed"]


def test_multiline_and_first_query(tmp_path):
    session_id = str(uuid.uuid4())
    history = HistoryClient(tmp_path, session_id)
    assert history.answer("first\nsecond").endswith("You previously asked:\n")
    assert history.answer("next").endswith("first\nsecond")
    assert len((tmp_path / "history.txt").read_text().splitlines()) == 2


def test_reject_invalid_session():
    with pytest.raises(ValueError):
        create_app(session_id="not-a-uuid")


def test_concurrent_appends_are_complete(tmp_path):
    history = HistoryClient(tmp_path, str(uuid.uuid4()))
    queries = [f"parallel-{number}" for number in range(20)]
    with ThreadPoolExecutor(max_workers=8) as workers:
        list(workers.map(history.answer, queries))
    records = [json.loads(record) for record in (tmp_path / "history.txt").read_text().splitlines()]
    assert sorted(records) == sorted(queries)


def test_fsync_failure_is_not_acknowledged(tmp_path, monkeypatch):
    def fail(_descriptor):
        raise OSError("disk sync failed")

    monkeypatch.setattr("app.os.fsync", fail)
    history = HistoryClient(tmp_path, str(uuid.uuid4()))
    with pytest.raises(OSError, match="disk sync failed"):
        history.answer("must not acknowledge")


def test_reject_continuation_and_empty_query(tmp_path):
    with TestClient(create_app(tmp_path, str(uuid.uuid4()))) as client:
        assert client.post("/v1/responses", json={"input": ""}).status_code == 400
        assert client.post("/v1/responses", json={"input": "hello", "previous_response_id": "other"}).status_code == 400
    assert not (tmp_path / "history.txt").exists()
