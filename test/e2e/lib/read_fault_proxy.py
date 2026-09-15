#!/usr/bin/env python3
"""Test-owned cache wire proxy. Faults affect only this listener's clients."""
import json
import pathlib
import signal
import socket
import struct
import sys
import threading

path, target, control, evidence = sys.argv[1:]
stop = threading.Event()
lock = threading.Lock()
connections = set()
workers = []


def frame(conn):
    def exact(size):
        chunks = bytearray()
        while len(chunks) < size:
            data = conn.recv(size - len(chunks))
            if not data:
                raise EOFError()
            chunks.extend(data)
        return chunks
    prefix = exact(4)
    length = struct.unpack("<I", prefix)[0]
    if length < 4 or length > 4 << 20:
        raise ValueError("invalid test frame")
    return prefix + exact(length - 4)


def record(mode, request):
    with lock, open(evidence, "a", encoding="utf8") as out:
        out.write(json.dumps({"mode": mode, "opcode": request[4],
                              "namespace": request[5]}) + "\n")


def serve(client):
    backend = None
    try:
        backend = socket.create_connection(("127.0.0.1", int(target)), timeout=10)
        backend.settimeout(None)
        with lock:
            connections.add(backend)
        while not stop.is_set():
            request = frame(client)
            mode = pathlib.Path(control).read_text().strip()
            chunk = request[4] == 1 and request[5] == 1
            if chunk and mode == "offline":
                record(mode, request)
                return  # one transport failure; caller must retain its request
            backend.sendall(request)
            response = frame(backend)
            if chunk and mode == "corrupt" and response[4] == 0:
                error_length = struct.unpack("<H", response[6:8])[0]
                response[8 + error_length] ^= 0x80
                record(mode, request)
            client.sendall(response)
    except (OSError, EOFError, ValueError):
        pass
    finally:
        for conn in (client, backend):
            if conn is not None:
                with lock:
                    connections.discard(conn)
                conn.close()


listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
listener.bind(path)
listener.listen()
listener.settimeout(0.2)
signal.signal(signal.SIGTERM, lambda *_: stop.set())
signal.signal(signal.SIGINT, lambda *_: stop.set())
try:
    while not stop.is_set():
        try:
            client, _ = listener.accept()
        except socket.timeout:
            continue
        with lock:
            connections.add(client)
        worker = threading.Thread(target=serve, args=(client,))
        # Completed test workers are joined during the run; the list is bounded
        # by active connections, including during a prolonged outage.
        remaining = []
        for old in workers:
            if old.is_alive():
                remaining.append(old)
            else:
                old.join()
        workers = remaining + [worker]
        worker.start()
finally:
    listener.close()
    with lock:
        for conn in connections:
            try:
                conn.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
    for worker in workers:
        worker.join()
    pathlib.Path(path).unlink(missing_ok=True)
