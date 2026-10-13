#!/usr/bin/env python3
"""Replay IPC frame scenarios against the Python DesktopIPC (the oracle).

Every outgoing frame (little-endian uint32 length prefix + compact JSON) is
captured hex-encoded. uuid4 is replaced with a deterministic counter; the
recorder socket auto-answers each request frame with a success response, so
the real request() path runs end-to-end without a desktop app.

Run:  python3 conformance/ipc/oracle.py [--goldens]
"""
import argparse
import json
import queue
import struct
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
import bridge.ipc as ipc_module  # noqa: E402


def uuid_text(counter):
    hex_text = format(counter, '032x')
    return f'{hex_text[:8]}-{hex_text[8:12]}-{hex_text[12:16]}-{hex_text[16:20]}-{hex_text[20:]}'


class FakeUuid:
    def __init__(self):
        self.counter = 0

    def uuid4(self):
        text = uuid_text(self.counter)
        self.counter += 1

        class Handle:
            def __str__(self):
                return text
        return Handle()


def frame_of(message):
    payload = json.dumps(message, ensure_ascii=False, separators=(',', ':')).encode()
    return struct.pack('<I', len(payload)) + payload


class Recorder:
    """Captures outgoing frames; auto-answers requests; serves an inbox."""

    def __init__(self):
        self.sent = []
        self.inbox = queue.Queue()
        self.buffer = b''
        self.dead = False

    def sendall(self, data):
        frame = bytes(data)
        self.sent.append(frame)
        message = json.loads(frame[4:])
        if message.get('type') == 'request':
            self.inbox.put(frame_of({
                'type': 'response', 'requestId': message['requestId'],
                'resultType': 'success',
                'result': {'clientId': 'desktop-9', 'handledByClientId': 'owner-7'}}))

    def recv(self, count):
        if self.dead:
            raise OSError('closed')
        while not self.buffer:
            item = self.inbox.get(timeout=5)
            if item is None:
                self.dead = True
                return b''
            self.buffer = item
        chunk, self.buffer = self.buffer[:count], self.buffer[count:]
        return chunk

    def settimeout(self, timeout):
        pass

    def shutdown(self, *args):
        pass

    def close(self):
        pass


def wait_for(condition, timeout=3.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return True
        time.sleep(0.01)
    return False


def run(scenario, emit):
    ipc_module.uuid = FakeUuid()
    recorder = Recorder()
    events = []
    instance = ipc_module.DesktopIPC('unused', on_event=events.append)
    instance.socket = recorder
    instance.client_id = 'client-abc'
    reader = threading.Thread(
        target=ipc_module.DesktopIPC._read_loop, args=(instance, recorder), daemon=True)
    reader.start()
    captured = 0
    for index, step in enumerate(scenario['steps']):
        operation = step['op']
        out = {'frame': None, 'event': None}
        if operation in ('request', 'follow'):
            try:
                if operation == 'request':
                    instance.request(step['method'], step['params'],
                                     target=step.get('target'),
                                     host=step.get('host', 'local'),
                                     timeout=step.get('timeout', 15))
                else:
                    instance.follow(step['session'], step.get('owner'),
                                    enabled=step.get('enabled', True),
                                    host=step.get('host', 'local'))
            except ipc_module.IPCError as error:
                out['error'] = str(error)
        elif operation == 'incoming':
            frames_before, events_before = len(recorder.sent), len(events)
            recorder.inbox.put(frame_of(step['message']))
            wait_for(lambda: len(recorder.sent) > frames_before or len(events) > events_before,
                     timeout=1.0)
        else:
            raise ValueError('unknown op ' + operation)
        frames = recorder.sent[captured:]
        captured += len(frames)
        if frames:
            out['frame'] = frames[0].hex()
        if events:
            out['event'] = events[-1]
        events.clear()
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': operation,
                         'out': out}, ensure_ascii=False, separators=(',', ':')).encode())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--goldens', action='store_true')
    arguments = parser.parse_args()
    scenario_dir = Path(__file__).resolve().parent / 'scenarios'
    lines = []
    for path in sorted(scenario_dir.glob('*.json')):
        run(json.loads(path.read_text(encoding='utf-8')), lines.append)
    payload = b''.join(line + b'\n' for line in lines)
    if arguments.goldens:
        (scenario_dir / 'golden.jsonl').write_bytes(payload)
        print(f'wrote {len(lines)} golden lines')
    else:
        sys.stdout.buffer.write(payload)


if __name__ == '__main__':
    main()
