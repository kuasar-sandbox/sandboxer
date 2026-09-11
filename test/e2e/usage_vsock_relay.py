"""Bounded test relay for faults on real usage frames; other traffic is raw."""
import json
import select
import socket
import socketserver
import struct
import threading
import time


def exact(connection, length):
    result = bytearray()
    while len(result) < length:
        data = connection.recv(length-len(result))
        if not data:
            raise EOFError()
        result.extend(data)
    return bytes(result)


def frame(connection):
    header = exact(connection, 4)
    length = struct.unpack("<I", header)[0]
    assert 0 < length <= 1 << 20, "invalid management frame length"
    payload = exact(connection, length)
    return header + payload, json.loads(payload)


def line(connection):
    value = bytearray()
    while len(value) < 128:
        value.extend(exact(connection, 1))
        if value[-1:] == b"\n":
            return bytes(value)
    raise ValueError("oversized vsock preface")


class UsageRelay(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    block_on_close = False

    def __init__(self, path, old_epoch=None):
        self.real = str(path) + ".real"
        self.lock = threading.Lock()
        self.stopping = threading.Event()
        self.connections = set()
        self.slots = threading.BoundedSemaphore(8)
        self.requests, self.errors = [], []
        self.next_connection, self.mode, self.first_response = 0, None, None
        self.old_epoch = old_epoch
        if old_epoch is not None:
            self.mode = "old-epoch"
        path.parent.mkdir(parents=True, exist_ok=True)
        super().__init__(str(path), UsageHandler)
        self.worker = threading.Thread(target=self.serve_forever, kwargs={"poll_interval": .1})
        self.worker.start()

    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False):
            self.errors.append("more than eight concurrent management connections")
            self.shutdown_request(request)
            return
        with self.lock:
            self.connections.add(request)
        super().process_request(request, client_address)

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            with self.lock:
                self.connections.discard(request)
            self.slots.release()

    def arm(self, mode):
        assert mode in ("drop", "delay", "repeat", "fragment")
        with self.lock:
            assert self.mode is None, "previous fault has not been consumed"
            self.mode = mode

    def stop(self):
        self.stopping.set()
        self.shutdown()
        self.worker.join(timeout=3)
        assert not self.worker.is_alive()
        with self.lock:
            connections = list(self.connections)
        for connection in connections:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self.server_close()
        end = time.monotonic() + 3
        while time.monotonic() < end:
            with self.lock:
                if not self.connections:
                    return
            time.sleep(.01)
        raise AssertionError("usage relay handlers did not exit")


class UsageHandler(socketserver.BaseRequestHandler):
    def handle(self):
        row, operation = None, "connect"
        upstream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        upstream.settimeout(3)
        self.request.settimeout(3)
        with self.server.lock:
            self.server.next_connection += 1
            number = self.server.next_connection
        try:
            end = time.monotonic() + 3
            while True:
                try:
                    upstream.connect(self.server.real)
                    break
                except (FileNotFoundError, ConnectionRefusedError):
                    if time.monotonic() >= end:
                        raise
                    time.sleep(.01)
            preface = line(self.request)
            upstream.sendall(preface)
            self.request.sendall(line(upstream))
            if preface != b"CONNECT 5000\n":
                self.raw(upstream)
                return
            data, message = frame(self.request)
            if message.get("type") != "usage_request":
                upstream.sendall(data)
                self.raw(upstream)
                return
            while True:
                assert message.get("type") == "usage_request", message
                row = {"connection": number, "request": message["usage_request"], "start_ns": time.monotonic_ns()}
                with self.server.lock:
                    row["mode"], self.server.mode = self.server.mode, None
                    self.server.requests.append(row)
                operation = "guest_send"
                upstream.sendall(data)
                operation = "guest_read"
                response, raw_message = frame(upstream)
                row.update(response=raw_message, received_ns=time.monotonic_ns())
                with self.server.lock:
                    if self.server.first_response is None and raw_message.get("type") == "usage_response":
                        self.server.first_response = response
                    first = self.server.first_response
                mode = row["mode"]
                if mode == "drop":
                    row["dropped"] = True
                    return
                if mode == "delay":
                    self.server.stopping.wait(1.4)
                    row["delay_released_ns"] = time.monotonic_ns()
                if mode == "old-epoch":
                    # Deliberate identity corruption, not an unmodified old
                    # frame: keep this real reply's values and request ID, but
                    # inject the actual previous run's epoch. Record both.
                    forwarded = json.loads(response[4:])
                    assert forwarded["usage_response"]["run_epoch"] != self.server.old_epoch
                    forwarded["usage_response"]["run_epoch"] = self.server.old_epoch
                    payload = json.dumps(forwarded).encode()
                    response = struct.pack("<I", len(payload)) + payload
                    row["forwarded"] = forwarded
                if mode == "repeat":
                    assert first is not None and first != response
                    response = first
                    row["forwarded"] = json.loads(first[4:])
                if mode == "fragment":
                    # The final fragment arrives after the original Host
                    # deadline, despite preceding successful partial reads.
                    operation = "host_send"
                    self.request.sendall(response[:4])
                    width = (len(response)-4+3)//4
                    row["fragments_sent_ns"] = []
                    for start in range(4, len(response), width):
                        self.server.stopping.wait(.4)
                        self.request.sendall(response[start:start+width])
                        row["fragments_sent_ns"].append(time.monotonic_ns())
                else:
                    operation = "host_send"
                    self.request.sendall(response)
                row["sent_ns"] = time.monotonic_ns()
                # Do not manufacture EOF after a bad reply. A broken client
                # which accepts it must remain observable on this connection.
                operation = "host_read"
                data, message = frame(self.request)
        except (EOFError, BrokenPipeError, ConnectionResetError):
            if row is not None and operation in ("host_read", "host_send"):
                row["client_closed_ns"] = time.monotonic_ns()
            elif row is not None and operation in ("guest_read", "guest_send"):
                row["guest_closed_ns"] = time.monotonic_ns()
        except OSError as error:
            if not self.server.stopping.is_set():
                self.server.errors.append(str(error))
        except BaseException as error:
            self.server.errors.append(str(error))
        finally:
            upstream.close()

    def raw(self, upstream):
        sockets = [self.request, upstream]
        while sockets and not self.server.stopping.is_set():
            ready, _, _ = select.select(sockets, [], [], .1)
            for source in ready:
                target = upstream if source is self.request else self.request
                data = source.recv(65536)
                if data:
                    target.sendall(data)
                else:
                    sockets.remove(source)
                    target.shutdown(socket.SHUT_WR)
