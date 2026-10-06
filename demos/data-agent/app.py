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

import time

_startup_origin = time.perf_counter_ns()
_startup_last_wall = _startup_origin
_startup_last_cpu = time.process_time_ns()

import asyncio
import fcntl
import json
import os
import sys
import uuid
from collections import deque
from contextlib import asynccontextmanager
from contextvars import ContextVar
from pathlib import Path

_startup_enabled = os.environ.get("AGENT_STARTUP_PROFILE") == "1"
_request_profile = ContextVar("request_profile", default=None)


def request_marker(stage):
    profile = _request_profile.get()
    if profile is None:
        return
    now = time.perf_counter_ns()
    print("AGENT_REQUEST " + json.dumps({"stage": stage, "wall_ms": (now - profile["last"]) / 1_000_000,
          "since_request_entry_ms": (now - profile["origin"]) / 1_000_000,
          "time_ns": time.time_ns()}, sort_keys=True), file=sys.stderr, flush=True)
    profile["last"] = now


def startup_marker(stage):
    global _startup_last_wall, _startup_last_cpu
    if not _startup_enabled:
        return
    wall = time.perf_counter_ns()
    cpu = time.process_time_ns()
    wall_time = time.time_ns()
    record = {
        "stage": stage,
        "wall_ms": (wall - _startup_last_wall) / 1_000_000,
        "cpu_ms": (cpu - _startup_last_cpu) / 1_000_000,
        "since_module_entry_ms": (wall - _startup_origin) / 1_000_000,
        "time_ns": wall_time,
    }
    entrypoint = os.environ.get("AGENT_ENTRYPOINT_START_NS")
    if entrypoint is not None:
        record["since_entrypoint_ms"] = (wall_time - int(entrypoint)) / 1_000_000
    _startup_last_wall, _startup_last_cpu = wall, cpu
    print("AGENT_STARTUP " + json.dumps(record, sort_keys=True), file=sys.stderr, flush=True)


startup_marker("stdlib_imports")
from agent_framework import Agent, BaseChatClient, ChatResponse, ChatResponseUpdate, Content, Message, ResponseStream
startup_marker("maf_core_imports")
from agent_framework_hosting_responses import create_response_id, responses_from_run, responses_from_streaming_run, responses_to_run
startup_marker("responses_adapter_imports")
from fastapi import FastAPI, HTTPException
from fastapi.responses import JSONResponse, StreamingResponse
startup_marker("fastapi_imports")


class HistoryClient(BaseChatClient):
    def __init__(self, home, session_id):
        super().__init__()
        self.home = Path(home)
        self.session_id = str(uuid.UUID(session_id))

    def answer(self, query):
        request_marker("history_thread_enter")
        self.home.mkdir(parents=True, exist_ok=True)
        with (self.home / "history.txt").open("a+", encoding="utf-8") as history:
            request_marker("home_mkdir_open")
            fcntl.flock(history, fcntl.LOCK_EX)
            request_marker("history_flock")
            history.seek(0)
            previous = [json.loads(record) for record in deque(history, maxlen=5)]
            request_marker("history_read")
            history.write(json.dumps(query, ensure_ascii=False) + "\n")
            history.flush()
            request_marker("history_append_flush")
            os.fsync(history.fileno())
            request_marker("history_fsync")
        return (
            f"You asked: {query}\nCurrent Session: {self.session_id}\n\n"
            "You previously asked:\n" + "\n".join(previous)
        )

    def _inner_get_response(self, *, messages, stream, options, **kwargs):
        query = next(message.text for message in reversed(messages) if message.role == "user")

        async def response():
            text = await asyncio.to_thread(self.answer, query)
            return ChatResponse(messages=[Message(role="assistant", contents=[text])])

        async def updates():
            text = await asyncio.to_thread(self.answer, query)
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text(text)])

        if stream:
            return ResponseStream(updates(), finalizer=ChatResponse.from_updates)
        return response()


def create_app(home=None, session_id=None):
    client = HistoryClient(home or Path.home(), session_id or os.environ["FOUNDRY_SESSION_ID"])
    startup_marker("history_client_init")
    agent = Agent(client=client, name="DataHistoryAgent")
    startup_marker("maf_agent_init")

    @asynccontextmanager
    async def lifespan(_application):
        startup_marker("asgi_lifespan_enter")
        yield

    application = FastAPI(lifespan=lifespan)
    startup_marker("fastapi_instance")
    boot_id = str(uuid.uuid4())
    first_ready = True

    @application.get("/readyz")
    async def ready():
        nonlocal first_ready
        if first_ready:
            startup_marker("first_ready_request")
            first_ready = False
        return {"session_id": client.session_id, "boot_id": boot_id}

    @application.post("/v1/responses", response_model=None)
    async def responses(body: dict):
        if _startup_enabled:
            origin = time.perf_counter_ns()
            _request_profile.set({"origin": origin, "last": origin})
        request_marker("request_handler_enter")
        if body.get("previous_response_id") or body.get("conversation") or body.get("tools"):
            raise HTTPException(400, "This agent uses its Actor HOME history, not Responses continuation or tools.")
        try:
            run = responses_to_run(body)
        except ValueError as error:
            raise HTTPException(400, str(error)) from error
        if not any(message.role == "user" and message.text for message in run["messages"]):
            raise HTTPException(400, "A nonempty text query is required.")
        response_id = create_response_id()
        headers = {"x-agent-boot-id": boot_id, "x-foundry-session-id": client.session_id}
        request_marker("responses_parse_validate")
        if run["stream"]:
            stream = agent.run(run["messages"], stream=True)
            return StreamingResponse(
                responses_from_streaming_run(stream, response_id=response_id),
                media_type="text/event-stream",
                headers=headers,
            )
        result = await agent.run(run["messages"])
        request_marker("maf_result_return")
        response = JSONResponse(responses_from_run(result, response_id=response_id), headers=headers)
        request_marker("responses_encode")
        return response

    startup_marker("routes_registered")
    return application


if __name__ == "__main__":
    startup_marker("module_definitions")
    import uvicorn
    startup_marker("uvicorn_import")

    application = create_app()
    startup_marker("uvicorn_run_enter")
    if _startup_enabled:
        class ProfiledServer(uvicorn.Server):
            async def startup(self, sockets=None):
                await super().startup(sockets=sockets)
                if self.started:
                    startup_marker("server_listening")

        ProfiledServer(uvicorn.Config(application, host="0.0.0.0", port=int(os.environ.get("PORT", "8080")))).run()
    else:
        uvicorn.run(application, host="0.0.0.0", port=int(os.environ.get("PORT", "8080")))
