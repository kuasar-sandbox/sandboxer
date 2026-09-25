"""Test-only LaunchPort observer; report ACK is delivery, not control completion."""
import socket
import threading
import time

from usage_vsock_relay import UsageHandler, UsageRelay, frame


class ReportRelay(UsageRelay):
    # Reuse the existing eight-connection limit, owned FDs and bounded stop.
    # This observer has no fault injection modes and no control-loop writes.
    def __init__(self, path):
        self.report_condition = threading.Condition()
        self.latest_report = None
        super().__init__(path)

    def finish_request(self, request, client_address):
        ReportHandler(request, client_address, self)

    def delivered(self, row):
        with self.report_condition:
            assert len(self.requests) < 64, "report-observation case exceeded its bound"
            self.requests.append(row)
            if row["response"].get("type") != "mem_report_ack":
                return
            report = row["request"]["mem_report"]
            key = int(report["epoch"]), int(report["seq"])
            prior = self.latest_report
            if prior is not None:
                old = prior["request"]["mem_report"]
                if key <= (int(old["epoch"]), int(old["seq"])):
                    return
            self.latest_report = row
            self.report_condition.notify_all()

    def next_report(self):
        with self.report_condition:
            previous = self.latest_report
            assert self.report_condition.wait_for(lambda: self.latest_report is not previous, timeout=7), "no fresh delivered memory report"
            return self.latest_report


class ReportHandler(UsageHandler):
    def handle(self):
        upstream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        upstream.settimeout(3)
        self.request.settimeout(3)
        try:
            upstream.connect(self.server.real)
            data, message = frame(self.request)
            started = time.monotonic_ns()
            upstream.sendall(data)
            if message.get("type") != "mem_report":
                # hello upgrades this very connection to stdio MUX; do not
                # parse or terminate it as a single request/response exchange.
                self.raw(upstream)
                return
            response, acknowledgement = frame(upstream)
            self.request.sendall(response)
            self.server.delivered({"request": message, "response": acknowledgement,
                                   "request_hex": data.hex(), "response_hex": response.hex(),
                                   "received_ns": started, "ack_forwarded_ns": time.monotonic_ns()})
        except (EOFError, BrokenPipeError, ConnectionResetError):
            pass
        except BaseException as error:
            if not self.server.stopping.is_set():
                self.server.errors.append(str(error))
        finally:
            upstream.close()
