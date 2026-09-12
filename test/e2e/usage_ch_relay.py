"""Bounded test-only relay: hold real CH replies, never invent actual/target."""
import http.client
import http.server
import json
import socket
import socketserver
import threading
import time


class CHRelay(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    block_on_close = False

    def __init__(self, path):
        self.real = str(path) + ".real"
        self.release = threading.Event()
        self.held = threading.Event()
        self.lock = threading.Lock()
        self.connections = set()
        self.slots = threading.BoundedSemaphore(4)
        self.requests, self.errors = [], []
        self.hold_path = None
        self.after_resize = False
        self.resize_below = None
        path.parent.mkdir(parents=True, exist_ok=True)
        super().__init__(str(path), CHHandler)
        self.worker = threading.Thread(target=self.serve_forever, kwargs={"poll_interval": .1})
        self.worker.start()

    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False):
            self.errors.append("more than four concurrent CH connections")
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

    def arm(self, path, after_resize=False, resize_below=None):
        with self.lock:
            assert self.hold_path is None and not self.held.is_set()
            self.hold_path = path
            self.after_resize = after_resize
            self.resize_below = resize_below

    def stop(self):
        self.release.set()
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
        raise AssertionError("CH relay handlers did not exit")


class CHHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_GET(self):
        self.forward()

    def do_PUT(self):
        self.forward()

    def forward(self):
        connection = http.client.HTTPConnection("localhost", timeout=10)
        row = {"method": self.command, "path": self.path, "start_ns": time.monotonic_ns()}
        with self.server.lock:
            self.server.requests.append(row)
        try:
            length = int(self.headers.get("Content-Length", "0"))
            assert 0 <= length <= 1 << 20
            body = self.rfile.read(length)
            row["request_body"] = body.decode()
            resize = self.command == "PUT" and self.path == "/api/v1/vm.resize"
            with self.server.lock:
                qualifying_resize = resize and (self.server.resize_below is None or
                    0 <= json.loads(body)["desired_balloon"] < self.server.resize_below)
                hold = self.server.hold_path == self.path and not self.server.after_resize and (not resize or qualifying_resize)
                if hold:
                    self.server.hold_path = None
            connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            connection.sock.settimeout(10)
            # The relay can be ready just before the wrapper execs CH.
            end = time.monotonic() + 5
            while True:
                try:
                    connection.sock.connect(self.server.real)
                    break
                except (FileNotFoundError, ConnectionRefusedError):
                    if time.monotonic() >= end:
                        raise
                    time.sleep(.01)
            connection.request(self.command, self.path, body=body,
                               headers={"Content-Type": "application/json", "Connection": "close"})
            response = connection.getresponse()
            raw = response.read(1 << 20)
            assert response.read(1) == b"", "oversized CH response"
            row.update(status=response.status, response_body=raw.decode(), received_ns=time.monotonic_ns(), held=hold)
            if hold:
                self.server.held.set()
                assert self.server.release.wait(timeout=20), "test failed to release CH response"
            row["released_ns"] = time.monotonic_ns()
            self.send_response(response.status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
            self.wfile.flush()
            if qualifying_resize and 200 <= response.status < 300:
                with self.server.lock:
                    self.server.after_resize = False
        except (BrokenPipeError, ConnectionResetError) as error:
            # A usage deadline is allowed to close its own connection while
            # the real reply is held. Preserve this in fault evidence.
            row["peer_closed"] = str(error)
            self.close_connection = True
        except BaseException as error:
            self.server.errors.append(str(error))
            self.close_connection = True
        finally:
            connection.close()
            row["end_ns"] = time.monotonic_ns()
