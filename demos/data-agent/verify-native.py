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

import argparse
import http.client
import json
import subprocess
import time

import httpx
from openai import OpenAI


class ActorTransport(httpx.BaseTransport):
    def __init__(self, atespace, actor, connect_port):
        self.target = f"{atespace}/{actor}"
        self.authority = f"{actor}.{atespace}"
        self.connect_port = connect_port

    def handle_request(self, request):
        connection = http.client.HTTPConnection("127.0.0.1", self.connect_port, timeout=180)
        headers = dict(request.headers)
        headers["ate-target-actor"] = self.target
        connection.set_tunnel(self.authority, 8080, headers={"ate-target-actor": self.target})
        try:
            connection.request(request.method, request.url.raw_path.decode(), body=request.read(), headers=headers)
            response = connection.getresponse()
            return httpx.Response(response.status, headers=response.getheaders(), content=response.read(), request=request)
        finally:
            connection.close()


def control(arguments, *command):
    result = subprocess.run([arguments.ate_cli, "--kubeconfig", arguments.kubeconfig, *command, "-o", "json"], check=True, capture_output=True, text=True)
    return json.loads(result.stdout)


def client_for(arguments, actor):
    http = httpx.Client(transport=ActorTransport(arguments.atespace, actor, arguments.connect_port), timeout=180)
    client = OpenAI(base_url="http://actor/v1", api_key="native-verification", http_client=http)
    return http, client


def actor_state(arguments, name):
    return control(arguments, "get", "actor", name, "-a", arguments.atespace)


def check_response(client, query, session_id, previous):
    result = client.responses.create(model="data-agent", input=query)
    expected = f"You asked: {query}\nCurrent Session: {session_id}\n\nYou previously asked:\n" + "\n".join(previous[-5:])
    assert result.output_text == expected, result.output_text
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--ate-cli", default="bin/kubectl-ate")
    parser.add_argument("--kubeconfig", default="/tmp/substrate-azure-data-kubeconfig")
    parser.add_argument("--atespace", default="ate-demo-data-agent")
    parser.add_argument("--actor", default="native-a")
    parser.add_argument("--other-actor", default="native-b")
    parser.add_argument("--connect-port", type=int, default=18081)
    parser.add_argument("--resume-only", action="store_true")
    parser.add_argument("--prior-boot")
    parser.add_argument("--expected-node")
    parser.add_argument("--benchmark-cycles", type=int, default=0)
    arguments = parser.parse_args()
    original = actor_state(arguments, arguments.actor)
    other = actor_state(arguments, arguments.other_actor)
    session = original["metadata"]["uid"]
    assert other["metadata"]["uid"] != session
    http, client = client_for(arguments, arguments.actor)
    other_http, other_client = client_for(arguments, arguments.other_actor)
    try:
        if arguments.benchmark_cycles:
            boot = http.get("http://actor/readyz").raise_for_status().json()
            expected_node = actor_state(arguments, arguments.actor)["status"]["workerAssignment"]["nodeName"]
            samples = []
            for _ in range(arguments.benchmark_cycles):
                started = time.monotonic()
                suspended = control(arguments, "suspend", "actor", arguments.actor, "-a", arguments.atespace)
                suspend_seconds = time.monotonic() - started
                assert suspended["status"]["externalSnapshot"]["contentScope"] == "SNAPSHOT_CONTENT_SCOPE_DATA"
                started = time.monotonic()
                resumed = control(arguments, "resume", "actor", arguments.actor, "-a", arguments.atespace)
                resume_seconds = time.monotonic() - started
                ready = http.get("http://actor/readyz").raise_for_status().json()
                assert ready["session_id"] == session and ready["boot_id"] != boot["boot_id"]
                assert resumed["status"]["workerAssignment"]["nodeName"] == expected_node
                samples.append({"suspend_cli_seconds": suspend_seconds, "resume_cli_seconds": resume_seconds, "boot_id": ready["boot_id"]})
                boot = ready
            print("NATIVE_DATA_BENCHMARK_OK " + json.dumps({"session_id": session, "node": expected_node, "samples": samples}, sort_keys=True))
            return
        if arguments.resume_only:
            if not arguments.prior_boot or not arguments.expected_node:
                raise ValueError("--resume-only requires --prior-boot and --expected-node")
            boot = {"session_id": session, "boot_id": arguments.prior_boot}
            history = [f"native-query-{number}" for number in range(1, 9)]
            suspended = original
            suspend_seconds = None
            expected_node = arguments.expected_node
        else:
            boot = http.get("http://actor/readyz").raise_for_status().json()
            assert boot["session_id"] == session
            history = []
            for number in range(1, 8):
                query = f"native-query-{number}"
                check_response(client, query, session, history)
                history.append(query)
            check_response(other_client, "isolated-query", other["metadata"]["uid"], [])
            text = ""
            with client.responses.create(model="data-agent", input="native-query-8", stream=True) as stream:
                for event in stream:
                    if event.type == "response.output_text.delta":
                        text += event.delta
            assert text.endswith("\n".join(history[-5:])), text
            history.append("native-query-8")
            started = time.monotonic()
            suspended = control(arguments, "suspend", "actor", arguments.actor, "-a", arguments.atespace)
            suspend_seconds = time.monotonic() - started
            expected_node = original["status"]["workerAssignment"]["nodeName"]
        assert suspended["status"]["state"] == "ACTOR_STATE_SUSPENDED"
        snapshot = suspended["status"]["externalSnapshot"]
        assert snapshot["contentScope"] == "SNAPSHOT_CONTENT_SCOPE_DATA", snapshot
        started = time.monotonic()
        resumed = control(arguments, "resume", "actor", arguments.actor, "-a", arguments.atespace)
        resume_seconds = time.monotonic() - started
        assert resumed["metadata"]["uid"] == session
        assert resumed["status"]["workerAssignment"]["nodeName"] == expected_node
        restored = http.get("http://actor/readyz").raise_for_status().json()
        assert restored["session_id"] == session
        assert restored["boot_id"] != boot["boot_id"]
        check_response(client, "after-same-node-resume", session, history)
        report = {"session_id": session, "node": resumed["status"]["workerAssignment"]["nodeName"], "first_boot": boot["boot_id"], "resumed_boot": restored["boot_id"], "snapshot": snapshot, "suspend_cli_seconds": suspend_seconds, "resume_cli_seconds": resume_seconds, "history_queries_before_resume": len(history), "isolated_actor_session": other["metadata"]["uid"]}
        print("NATIVE_DATA_SAME_NODE_OK " + json.dumps(report, sort_keys=True))
    finally:
        http.close()
        other_http.close()


if __name__ == "__main__":
    main()