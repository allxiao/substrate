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

import httpx
from openai import OpenAI


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--session-id", required=True)
    arguments = parser.parse_args()
    readiness = httpx.get(arguments.url + "/readyz")
    readiness.raise_for_status()
    assert readiness.json()["session_id"] == arguments.session_id
    client = OpenAI(base_url=arguments.url + "/v1", api_key="local-verification")
    for query_number in range(1, 8):
        result = client.responses.create(model="data-agent", input=f"query-{query_number}")
        previous = "\n".join(f"query-{number}" for number in range(max(1, query_number - 5), query_number))
        expected = f"You asked: query-{query_number}\nCurrent Session: {arguments.session_id}\n\nYou previously asked:\n" + previous
        assert result.output_text == expected, result.output_text
    text = ""
    with client.responses.create(model="data-agent", input="query-8", stream=True) as stream:
        for event in stream:
            if event.type == "response.output_text.delta":
                text += event.delta
    assert text.endswith("\n".join(f"query-{number}" for number in range(3, 8))), text
    print(f"AGENT_RESPONSES_OK session={arguments.session_id} boot={readiness.json()['boot_id']} nonstream=7 stream=1")


if __name__ == "__main__":
    main()
