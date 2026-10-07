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

import http.client
import importlib.util
import json
import os
import resource
import shutil
import subprocess
import struct
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path


def counters():
    paths = [Path('/sys/fs/cgroup')]
    membership = Path('/proc/self/cgroup').read_text()
    for line in membership.splitlines():
        if line.startswith('0::'):
            location = Path('/sys/fs/cgroup') / line[3:].lstrip('/')
            while location != Path('/sys/fs/cgroup') and location.is_relative_to('/sys/fs/cgroup'):
                paths.append(location)
                location = location.parent
    result = {'membership': membership, 'guest_cpu': Path('/proc/stat').read_text().splitlines()[0],
              'diskstats': Path('/proc/diskstats').read_text()}
    for location in paths:
        for name in ('cpu.max', 'cpu.stat', 'cpu.pressure', 'memory.max', 'memory.current', 'memory.events'):
            path = location / name
            if path.exists():
                result[str(path)] = path.read_text()
    return result


def measure_files(paths, operation):
    before = resource.getrusage(resource.RUSAGE_SELF)
    start = time.perf_counter_ns()
    byte_count = 0
    for path in paths:
        if operation == 'stat':
            byte_count += path.stat().st_size
        elif operation == 'read':
            byte_count += len(path.read_bytes())
        elif operation == 'open_close':
            path.open('rb').close()
        else:
            raise ValueError(operation)
    after = resource.getrusage(resource.RUSAGE_SELF)
    return {
        'wall_ms': (time.perf_counter_ns() - start) / 1e6,
        'user_ms': (after.ru_utime - before.ru_utime) * 1000,
        'system_ms': (after.ru_stime - before.ru_stime) * 1000,
        'files': len(paths), 'bytes': byte_count,
    }


def measure_held_reads(paths):
    if len(paths) > 128:
        batches = [measure_held_reads(paths[offset:offset + 128]) for offset in range(0, len(paths), 128)]
        return [{'cycle': cycle, 'files': len(paths),
                 **{key: sum(batch[cycle][key] for batch in batches)
                    for key in ('wall_ms', 'user_ms', 'system_ms', 'bytes')}}
                for cycle in range(3)]
    descriptors = []
    try:
        for path in paths:
            descriptor = os.open(path, os.O_RDONLY)
            descriptors.append((descriptor, os.fstat(descriptor).st_size))
        results = []
        for cycle in range(3):
            before = resource.getrusage(resource.RUSAGE_SELF)
            start = time.perf_counter_ns()
            byte_count = sum(len(os.pread(descriptor, size, 0)) for descriptor, size in descriptors)
            after = resource.getrusage(resource.RUSAGE_SELF)
            results.append({'cycle': cycle, 'wall_ms': (time.perf_counter_ns() - start) / 1e6,
                            'user_ms': (after.ru_utime - before.ru_utime) * 1000,
                            'system_ms': (after.ru_stime - before.ru_stime) * 1000,
                            'files': len(paths), 'bytes': byte_count})
        return results
    finally:
        for descriptor, _size in descriptors:
            os.close(descriptor)


def prepare_ram_site(source, target):
    entries = [path for path in source.iterdir()
               if path.name not in ('pip', 'pygments', '_pytest', 'pytest')
               and not (path.is_dir() and any(path.rglob('*.so')))
               and path.suffix != '.so']
    files = [file for entry in entries for file in ([entry] if entry.is_file() else entry.rglob('*')) if file.is_file()]
    needed = sum(((file.stat().st_size + 4095) // 4096) * 4096 for file in files)
    available = os.statvfs(target.parent)
    assert needed + 8 * 1024 * 1024 < available.f_bavail * available.f_frsize
    target.mkdir()
    for entry in entries:
        if entry.is_dir():
            shutil.copytree(entry, target / entry.name)
        else:
            shutil.copy2(entry, target / entry.name)
    return {'files': len(files), 'allocated_file_bytes': needed,
            'excluded': sorted(path.name for path in source.iterdir() if path not in entries)}


def inspect_bytecode(source, repair=False):
    result = {'valid_timestamp': 0, 'mtime_mismatch': 0, 'size_mismatch': 0,
              'hash_based': 0, 'unsupported': 0, 'repaired': 0, 'samples': []}
    for cache in sorted(source.rglob('*.pyc')):
        try:
            original = Path(importlib.util.source_from_cache(str(cache)))
        except ValueError:
            result['unsupported'] += 1
            continue
        if not original.is_file():
            result['unsupported'] += 1
            continue
        with cache.open('rb') as file:
            header = file.read(16)
        if len(header) != 16:
            result['unsupported'] += 1
            continue
        magic, flags, timestamp, size = struct.unpack('<4sIII', header)
        if magic != importlib.util.MAGIC_NUMBER:
            result['unsupported'] += 1
            continue
        if flags & 1:
            result['hash_based'] += 1
            continue
        status = original.stat()
        if status.st_size != size:
            result['size_mismatch'] += 1
        elif int(status.st_mtime) & 0xffffffff == timestamp:
            result['valid_timestamp'] += 1
        else:
            result['mtime_mismatch'] += 1
            if len(result['samples']) < 5:
                result['samples'].append({'source': str(original), 'source_mtime': int(status.st_mtime), 'pyc_mtime': timestamp, 'size': size})
            if repair:
                os.utime(original, ns=(status.st_atime_ns, timestamp * 1_000_000_000))
                result['repaired'] += 1
    return result


def profile_app(environment):
    before = resource.getrusage(resource.RUSAGE_CHILDREN)
    start = time.perf_counter_ns()
    process = subprocess.Popen([sys.executable, '/app/app.py'], env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    lines = []
    try:
        for line in process.stderr:
            lines.append(line)
            if 'Uvicorn running on ' in line:
                connection = http.client.HTTPConnection('127.0.0.1', 8081, timeout=5)
                connection.request('GET', '/readyz')
                response = connection.getresponse()
                assert response.status == 200
                response.read()
                connection.close()
                ready_ms = (time.perf_counter_ns() - start) / 1e6
                break
        else:
            raise RuntimeError('Agent exited before listening: ' + ''.join(lines))
    finally:
        process.terminate()
        _unused, remaining = process.communicate(timeout=10)
        lines.append(remaining or '')
    after = resource.getrusage(resource.RUSAGE_CHILDREN)
    markers = [json.loads(line.removeprefix('AGENT_STARTUP ')) for line in ''.join(lines).splitlines() if line.startswith('AGENT_STARTUP ')]
    assert markers[-1]['stage'] == 'first_ready_request'
    return {
        'spawn_to_ready_ms': ready_ms, 'markers': markers,
        'lifetime_user_ms': (after.ru_utime - before.ru_utime) * 1000,
        'lifetime_system_ms': (after.ru_stime - before.ru_stime) * 1000,
        'maxrss_kib': after.ru_maxrss,
        'minor_faults': after.ru_minflt - before.ru_minflt,
        'major_faults': after.ru_majflt - before.ru_majflt,
        'voluntary_switches': after.ru_nvcsw - before.ru_nvcsw,
        'involuntary_switches': after.ru_nivcsw - before.ru_nivcsw,
    }


def profile_ram_apps(environment, emit, ram_case='tmpfs_site', root_case='rootfs_return'):
    ram_site = Path('/dev/shm/loading-site')
    start = time.perf_counter_ns()
    copied = prepare_ram_site(Path('/usr/local/lib/python3.12/site-packages'), ram_site)
    emit({'kind': 'ram_copy', 'wall_ms': (time.perf_counter_ns() - start) / 1e6, **copied})
    settings = dict(environment, PYTHONPATH=str(ram_site))
    origins = "import importlib.util,json; print(json.dumps({name:importlib.util.find_spec(name).origin for name in ('agent_framework','agent_framework_hosting_responses','openai','pydantic','pydantic_core')}))"
    emit({'kind': 'ram_resolution', 'origins': json.loads(subprocess.check_output([sys.executable, '-c', origins], env=settings, text=True))})
    for case in (ram_case, root_case):
        settings = dict(environment, PYTHONPATH=str(ram_site)) if case == ram_case else environment
        for cycle in range(3):
            before = counters()
            result = profile_app(settings)
            emit({'kind': 'app', 'case': case, 'cycle': cycle, 'time_ns': time.time_ns(),
                  'before': before, 'after': counters(), **result})


def run(emit):
    emit({'kind': 'environment', 'time_ns': time.time_ns(), 'uname': list(os.uname()),
          'affinity': sorted(os.sched_getaffinity(0)), 'counters': counters(),
            'rlimit_nofile': list(resource.getrlimit(resource.RLIMIT_NOFILE)),
            'meminfo': Path('/proc/meminfo').read_text().splitlines()[:5],
          'mountinfo': Path('/proc/self/mountinfo').read_text(),
          'cpuinfo': Path('/proc/cpuinfo').read_text().split('\n\n')[0]})
    environment = dict(os.environ, PORT='8081', AGENT_STARTUP_PROFILE='1')
    compute_only = os.environ.get('LOADING_AB_COMPUTE_ONLY') == '1'
    if not compute_only:
        for cycle in range(3):
            before = counters()
            result = profile_app(environment)
            emit({'kind': 'app', 'case': 'rootfs', 'cycle': cycle, 'time_ns': time.time_ns(),
                  'before': before, 'after': counters(), **result})
    if os.environ.get('LOADING_AB_BYTECODE_REPAIR') == '1':
        source = Path('/usr/local/lib/python3.12')
        emit({'kind': 'bytecode_before', **inspect_bytecode(source)})
        start = time.perf_counter_ns()
        repaired = inspect_bytecode(source, repair=True)
        emit({'kind': 'bytecode_repair', 'wall_ms': (time.perf_counter_ns() - start) / 1e6, **repaired})
        emit({'kind': 'bytecode_after', **inspect_bytecode(source)})
        for cycle in range(3):
            before = counters()
            result = profile_app(environment)
            emit({'kind': 'app', 'case': 'valid_bytecode', 'cycle': cycle, 'time_ns': time.time_ns(),
                  'before': before, 'after': counters(), **result})
        if os.environ.get('LOADING_AB_RAM_SITE') == '1':
            profile_ram_apps(environment, emit, 'valid_bytecode_tmpfs', 'valid_bytecode_return')
        emit({'kind': 'complete', 'time_ns': time.time_ns(), 'counters': counters()})
        return
    computation = """import hashlib,json,time,resource
from pydantic import BaseModel,create_model
class Item(BaseModel):
    value: int
    label: str
    amounts: list[int]
results={}
operations=[('python',lambda:sum(number*number for number in range(3000000))),('openssl',lambda:hashlib.pbkdf2_hmac('sha256',b'password',b'salt',500000)),('objects',lambda:[{'value':number,'amounts':[number]*4} for number in range(100000)]),('pydantic_instances',lambda:[Item(value=number,label='fixed',amounts=[1,2,3]) for number in range(50000)]),('pydantic_schemas',lambda:[create_model(f'Model{number}',value=(int,...),label=(str,...),amounts=(list[int],...)) for number in range(500)])]
for name,operation in operations:
    before=resource.getrusage(resource.RUSAGE_SELF); start=time.perf_counter_ns(); operation(); after=resource.getrusage(resource.RUSAGE_SELF)
    results[name]={'wall_ms':(time.perf_counter_ns()-start)/1e6,'user_ms':(after.ru_utime-before.ru_utime)*1000,'system_ms':(after.ru_stime-before.ru_stime)*1000}
print(json.dumps(results))
"""
    for cycle in range(3):
        emit({'kind': 'compute', 'cycle': cycle, 'results': json.loads(subprocess.check_output([sys.executable, '-c', computation], text=True))})
    if compute_only:
        emit({'kind': 'complete', 'time_ns': time.time_ns(), 'counters': counters()})
        return
    source = Path('/usr/local/lib/python3.12')
    paths = sorted(source.rglob('*.pyc'))[:2000]
    assert len(paths) == 2000
    for operation in ('stat', 'read', 'open_close'):
        for cycle in range(3):
            emit({'kind': 'files', 'case': 'rootfs', 'operation': operation, 'cycle': cycle, **measure_files(paths, operation)})
    for result in measure_held_reads(paths):
        emit({'kind': 'files', 'case': 'rootfs', 'operation': 'read_held', **result})
    target = Path('/dev/shm/loading-ab')
    needed = sum(path.stat().st_size for path in paths)
    available = os.statvfs(target.parent)
    assert needed < available.f_bavail * available.f_frsize
    copies = []
    for path in paths:
        copied = target / path.relative_to(source)
        copied.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(path, copied)
        copies.append(copied)
    for operation in ('stat', 'read', 'open_close'):
        for cycle in range(3):
            emit({'kind': 'files', 'case': 'tmpfs', 'operation': operation, 'cycle': cycle, **measure_files(copies, operation)})
    for result in measure_held_reads(copies):
        emit({'kind': 'files', 'case': 'tmpfs', 'operation': 'read_held', **result})
    shutil.rmtree(target)
    if os.environ.get('LOADING_AB_RAM_SITE') == '1':
        profile_ram_apps(environment, emit)
    emit({'kind': 'complete', 'time_ns': time.time_ns(), 'counters': counters()})


if __name__ == '__main__':
    with Path('/tmp/loading-ab.jsonl').open('w') as output:
        def emit(record):
            output.write(json.dumps(record, sort_keys=True) + '\n')
            output.flush()
            if os.environ.get('LOADING_AB_STDOUT') == '1':
                print('LOADING_AB_RECORD ' + json.dumps(record, sort_keys=True), flush=True)
        try:
            run(emit)
        except Exception as error:
            emit({'kind': 'error', 'error': repr(error)})
            raise

    class ReadyHandler(BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200 if self.path == '/readyz' else 404)
            self.end_headers()

    HTTPServer(('0.0.0.0', 8080), ReadyHandler).serve_forever()
