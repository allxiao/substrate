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

FROM python:3.12-slim@sha256:02108f5d322dd89f1c9e552442c25acb0543dfdbc455693a5599624f20d9155d
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 HOME=/home/agent PORT=8080
WORKDIR /app
COPY requirements.lock .
RUN --mount=from=wheels,target=/wheels pip install --no-index --find-links=/wheels --no-cache-dir -r requirements.lock && mkdir -p /home/agent
COPY app.py .
COPY profile-start.sh .
EXPOSE 8080
ENTRYPOINT ["python", "/app/app.py"]
