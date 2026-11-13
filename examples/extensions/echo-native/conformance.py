"""Exercise the real shared-library ABI with an in-memory Python host."""

import base64
import ctypes
import json
import os
from pathlib import Path
import queue
import sys
import threading
import time
import unittest


MAX_FRAME_BYTES = 24 << 20
MAX_CHUNK_BYTES = 64 << 10
EMIT = ctypes.CFUNCTYPE(
    ctypes.c_int32, ctypes.c_size_t, ctypes.POINTER(ctypes.c_uint8), ctypes.c_size_t
)


class Descriptor(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("struct_size", ctypes.c_uint32),
        ("host", ctypes.c_size_t),
        ("emit", EMIT),
    ]


class Host:
    def __init__(self, directory):
        manifest = json.loads((directory / "magpie-plugin.json").read_text())
        self.manifest = manifest
        self.library = ctypes.CDLL(str(directory / manifest["entry"]))
        self.open = self.library.magpie_extension_open_v1
        self.open.argtypes = [ctypes.POINTER(Descriptor), ctypes.POINTER(ctypes.c_uint64)]
        self.open.restype = ctypes.c_int32
        self.send = self.library.magpie_extension_send_v1
        self.send.argtypes = [
            ctypes.c_uint64, ctypes.POINTER(ctypes.c_uint8), ctypes.c_size_t
        ]
        self.send.restype = ctypes.c_int32
        self.close = self.library.magpie_extension_close_v1
        self.close.argtypes = [ctypes.c_uint64]
        self.close.restype = ctypes.c_int32
        self.lock = threading.Lock()
        self.next = 0
        self.peers = {}
        self.errors = []
        self.emit = EMIT(self.receive)

    def receive(self, opaque, data, length):
        try:
            if length > MAX_FRAME_BYTES + 1:
                raise ValueError("oversized native callback")
            with self.lock:
                peer = self.peers.get(opaque)
            if peer is None:
                return 1
            body = ctypes.string_at(data, length)
            peer.frames.put(json.loads(body))
            return 0
        except Exception as error:
            self.errors.append(str(error))
            return 1

    def session(self):
        with self.lock:
            self.next += 1
            opaque = self.next
            peer = Peer(self, opaque)
            self.peers[opaque] = peer
        descriptor = Descriptor(1, ctypes.sizeof(Descriptor), opaque, self.emit)
        handle = ctypes.c_uint64()
        if self.open(ctypes.byref(descriptor), ctypes.byref(handle)) != 0:
            raise AssertionError("native Open failed")
        peer.handle = handle.value
        # The library must copy both the opaque integer and callback descriptor.
        descriptor.host = 0
        descriptor.emit = EMIT()
        return peer


class Peer:
    def __init__(self, host, opaque):
        self.host = host
        self.opaque = opaque
        self.handle = 0
        self.frames = queue.Queue()
        self.closed = False

    def send(self, frame):
        body = json.dumps(frame, separators=(",", ":")).encode() + b"\n"
        buffer = ctypes.create_string_buffer(body)
        status = self.host.send(
            self.handle, ctypes.cast(buffer, ctypes.POINTER(ctypes.c_uint8)), len(body)
        )
        ctypes.memset(buffer, 0x21, len(body))
        if status != 0:
            raise AssertionError("native Send failed")

    def call(self, identifier, tool, arguments=None, **extra):
        self.send({
            "kind": "call", "id": identifier, "method": "tool.call",
            "scope": {
                "plugin": "sdk-native-echo", "session": str(self.opaque),
                "operation": identifier, "attempt": 1,
            },
            "params": {"id": tool, "arguments": arguments or {}},
            **extra,
        })

    def read(self):
        return self.frames.get(timeout=5)

    def ack(self, frame):
        self.send({"kind": "ack", "id": frame["id"], "seq": frame["seq"]})

    def close(self):
        if not self.closed:
            self.closed = True
            with self.host.lock:
                self.host.peers.pop(self.opaque, None)
            if self.host.close(self.handle) != 0:
                raise AssertionError("native Close failed")


class Conformance(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.host = Host(DISTRIBUTION)

    def setUp(self):
        self.peer = self.host.session()
        self.addCleanup(self.peer.close)

    def tearDown(self):
        self.assertEqual(self.host.errors, [])

    def result(self, frame):
        self.assertEqual(frame["kind"], "result")
        self.assertNotIn("error", frame)
        return json.loads(frame["result"]["content"][0]["text"])

    def pid(self, peer, identifier="pid"):
        peer.call(identifier, "pid", {"text": "native"})
        result = self.result(peer.read())
        self.assertEqual(result, {"pid": os.getpid(), "text": "native"})

    def test_host_pid_and_public_handshake(self):
        self.assertTrue(self.peer.frames.empty())
        self.peer.send({"kind": "call", "id": "describe", "method": "sdk.describe"})
        frame = self.peer.read()
        expected = {
            key: value for key, value in self.host.manifest.items()
            if key not in ("runtime", "entry", "args")
        }
        self.assertEqual(frame["result"], expected)
        self.pid(self.peer)

    def test_concurrent_sessions_can_reuse_call_ids(self):
        second = self.host.session()
        self.addCleanup(second.close)
        self.peer.call("same-id", "pid")
        second.call("same-id", "pid")
        self.assertEqual(self.result(self.peer.read())["pid"], os.getpid())
        self.assertEqual(self.result(second.read())["pid"], os.getpid())
        second.close()
        self.pid(self.peer, "still-open")

    def test_stream_ack_backpressure_and_chunk_bounds(self):
        text = "n" * (MAX_CHUNK_BYTES + 3)
        self.peer.call("stream", "stream", {"text": text})
        accepted = self.peer.read()
        self.assertEqual(accepted["event"], "accepted")
        self.assertEqual(accepted["data"], {"operationId": "stream"})
        self.peer.ack(accepted)
        first = self.peer.read()
        self.assertEqual(first["event"], "data")
        self.assertEqual(base64.b64decode(first["data"]), b"n" * MAX_CHUNK_BYTES)
        self.peer.send({"kind": "call", "id": "barrier", "method": "sdk.describe"})
        self.assertEqual(self.peer.read()["id"], "barrier")
        self.peer.ack(first)
        second = self.peer.read()
        self.assertEqual(base64.b64decode(second["data"]), b"nnn")
        self.peer.ack(second)
        flushed = self.peer.read()
        self.assertEqual(flushed["event"], "flush")
        self.peer.ack(flushed)
        self.assertEqual(self.result(self.peer.read())["text"], text)

    def test_scoped_state_get_and_cas(self):
        self.peer.call("state", "state", {"key": "echo", "value": {"native": True}})
        self.peer.ack(self.peer.read())
        get = self.peer.read()
        self.assertEqual(get["kind"], "host.call")
        self.assertEqual(get["parent"], "state")
        self.assertEqual(get["method"], "state.get")
        self.assertEqual(get["scope"]["session"], str(self.peer.opaque))
        self.peer.send({
            "kind": "host.result", "id": get["id"], "parent": "state",
            "result": {"found": False},
        })
        cas = self.peer.read()
        self.assertEqual(cas["method"], "state.cas")
        self.assertEqual(cas["params"], {"key": "echo", "value": {"native": True}})
        self.peer.send({
            "kind": "host.result", "id": cas["id"], "parent": "state",
            "result": {"swapped": True, "version": "1"},
        })
        self.assertEqual(self.result(self.peer.read()), {"swapped": True, "version": "1"})

    def test_cancel_with_unacked_event_and_late_ack(self):
        self.peer.call("cancel", "wait")
        event = self.peer.read()
        self.peer.send({"kind": "cancel", "id": "cancel"})
        terminal = self.peer.read()
        self.assertEqual(terminal["error"]["code"], "canceled")
        self.peer.ack(event)
        self.pid(self.peer, "after-cancel")

    def test_cancel_host_callback_and_late_reply(self):
        self.peer.call("cancel", "state")
        self.peer.ack(self.peer.read())
        pending = self.peer.read()
        self.peer.send({"kind": "cancel", "id": "cancel"})
        self.assertEqual(self.peer.read()["error"]["code"], "canceled")
        self.peer.send({
            "kind": "host.result", "id": pending["id"], "parent": "cancel",
            "result": {"found": False},
        })
        self.pid(self.peer, "after-callback-cancel")

    def test_deadline_and_unknown_tool_are_public_errors(self):
        self.peer.call("deadline", "wait", deadline=int(time.time() * 1000) - 1000)
        self.assertEqual(self.peer.read()["error"]["code"], "deadline")
        self.peer.call("invalid", "undeclared")
        error = self.peer.read()["error"]
        self.assertEqual(error["code"], "invalid_params")
        self.assertEqual(error["outcome"], "not-started")
        self.pid(self.peer, "after-errors")

    def test_close_revokes_pending_call_without_waiting(self):
        self.peer.call("pending", "wait")
        self.assertEqual(self.peer.read()["event"], "accepted")
        closed = threading.Event()
        errors = []

        def close():
            try:
                self.peer.close()
            except Exception as error:
                errors.append(str(error))
            finally:
                closed.set()

        thread = threading.Thread(target=close, daemon=True)
        thread.start()
        self.assertTrue(closed.wait(5), "native Close blocked")
        thread.join()
        self.assertEqual(errors, [])
        self.assertNotEqual(self.host.send(self.peer.handle, None, 0), 0)
        self.assertNotEqual(self.host.close(self.peer.handle), 0)
        second = self.host.session()
        self.addCleanup(second.close)
        self.pid(second)

    def test_invalid_descriptors_and_spans(self):
        for version, size, emit in [
            (0, ctypes.sizeof(Descriptor), self.host.emit),
            (1, 0, self.host.emit),
            (1, ctypes.sizeof(Descriptor), EMIT()),
        ]:
            descriptor = Descriptor(version, size, 1, emit)
            handle = ctypes.c_uint64(999)
            self.assertNotEqual(self.host.open(ctypes.byref(descriptor), ctypes.byref(handle)), 0)
            self.assertEqual(handle.value, 0)
        self.assertNotEqual(self.host.open(None, None), 0)
        self.assertNotEqual(self.host.send(self.peer.handle, None, 1), 0)
        self.assertNotEqual(self.host.send(self.peer.handle, None, MAX_FRAME_BYTES + 2), 0)
        self.assertNotEqual(self.host.send(0, None, 0), 0)
        self.pid(self.peer, "after-invalid-spans")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: python3 conformance.py DISTRIBUTION_DIRECTORY")
    DISTRIBUTION = Path(sys.argv[1]).resolve()
    unittest.main(argv=[sys.argv[0]], verbosity=2)
