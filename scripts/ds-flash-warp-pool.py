#!/usr/bin/env python3
"""Local, userspace Cloudflare WARP pool for the DS Flash helper.

Each profile is a separate `wireproxy` process: WireGuard runs entirely in
userspace and exposes an HTTP CONNECT proxy on loopback.  No TUN device,
default route, DNS setting or other host-wide network state is changed.

The helper talks only to 127.0.0.1:<listen>.  When every route reports a
free-usage rejection, the helper posts to /rotate; this manager restarts each
profile so WireGuard performs a fresh registration handshake and receives a
new Cloudflare egress address.  The helper then retries the request once.

Configuration is deliberately file/env based so the private WireGuard
profiles never appear in command lines or logs.
"""

from __future__ import annotations

import json
import os
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HOME = Path.home()
WARP_DIR = Path(os.environ.get("WARP_POOL_DIR", HOME / ".config/opencode-cc/warp"))
STATE_DIR = Path(os.environ.get("WARP_POOL_STATE", HOME / ".local/state/opencode-cc-warp-pool"))
WIREPROXY = Path(os.environ.get("WIREPROXY_BIN", HOME / ".local/bin/wireproxy"))
CONTROL_HOST = "127.0.0.1"
CONTROL_PORT = int(os.environ.get("WARP_POOL_CONTROL_PORT", "18890"))
BASE_PORT = int(os.environ.get("WARP_POOL_BASE_PORT", "18891"))
TRACE_URL = "https://www.cloudflare.com/cdn-cgi/trace"
START_TIMEOUT = 25.0
STOP_TIMEOUT = 5.0
ROTATE_ATTEMPTS = 3


def log(event: str, **fields: object) -> None:
    print(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                      "event": event, **fields}), flush=True)


def trace_via(port: int, timeout: float = 12.0) -> dict[str, str]:
    proxy = f"http://127.0.0.1:{port}"
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({"http": proxy, "https": proxy})
    )
    with opener.open(TRACE_URL, timeout=timeout) as response:
        raw = response.read(16384).decode("utf-8", "replace")
    result: dict[str, str] = {}
    for line in raw.splitlines():
        key, sep, value = line.partition("=")
        if sep and key in {"ip", "warp", "colo"}:
            result[key] = value.strip()
    return result


class Profile:
    def __init__(self, index: int, wg: Path):
        self.index = index
        self.wg = wg
        self.name = f"warp{index}"
        self.port = BASE_PORT + index - 1
        self.config = STATE_DIR / f"wireproxy-{index}.conf"
        self.log = STATE_DIR / f"wireproxy-{index}.log"
        self.proc: subprocess.Popen[bytes] | None = None
        self.ip: str | None = None

    def write_config(self) -> None:
        self.config.write_text(
            f"WGConfig = {self.wg}\n\n[http]\nBindAddress = 127.0.0.1:{self.port}\n",
            encoding="utf-8",
        )
        os.chmod(self.config, 0o600)

    def start(self) -> None:
        if self.proc is not None and self.proc.poll() is None:
            return
        self.write_config()
        self.log.touch(mode=0o600, exist_ok=True)
        output = open(self.log, "ab", buffering=0)
        try:
            self.proc = subprocess.Popen(
                [str(WIREPROXY), "--config", str(self.config)],
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=subprocess.STDOUT,
                start_new_session=True,
                close_fds=True,
            )
        finally:
            output.close()
        log("profile_started", profile=self.name, port=self.port, pid=self.proc.pid)

    def stop(self) -> None:
        proc = self.proc
        self.proc = None
        if proc is None:
            return
        if proc.poll() is None:
            try:
                proc.terminate()
                proc.wait(timeout=STOP_TIMEOUT)
            except subprocess.TimeoutExpired:
                proc.kill()
                try:
                    proc.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    pass
            except ProcessLookupError:
                pass
        log("profile_stopped", profile=self.name, returncode=proc.returncode)

    def wait_ready(self) -> dict[str, str]:
        deadline = time.monotonic() + START_TIMEOUT
        last_error = ""
        while time.monotonic() < deadline:
            if self.proc is not None and self.proc.poll() is not None:
                last_error = f"wireproxy exited {self.proc.returncode}"
                break
            try:
                with socket.create_connection(("127.0.0.1", self.port), timeout=1.0):
                    pass
                info = trace_via(self.port)
                if info.get("warp") == "on" and info.get("ip"):
                    self.ip = info["ip"]
                    log("profile_ready", profile=self.name, port=self.port,
                        ip=self.ip, colo=info.get("colo"))
                    return info
                last_error = f"trace={info}"
            except Exception as error:  # startup race, handshake, DNS, TLS
                last_error = str(error)[:160]
            time.sleep(0.5)
        raise RuntimeError(f"{self.name} did not become ready: {last_error}")

    def health(self) -> dict[str, object]:
        running = self.proc is not None and self.proc.poll() is None
        result: dict[str, object] = {"name": self.name, "port": self.port,
                                     "running": running, "ip": self.ip}
        if running:
            try:
                info = trace_via(self.port, timeout=8)
                result.update(info)
            except Exception as error:
                result["error"] = str(error)[:160]
        return result


class Pool:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        STATE_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
        if not WIREPROXY.is_file() or not os.access(WIREPROXY, os.X_OK):
            raise RuntimeError(f"wireproxy is not executable: {WIREPROXY}")
        files = sorted(WARP_DIR.glob("wgcf-profile-*.conf"))
        if not files:
            raise RuntimeError(f"no WARP profiles under {WARP_DIR}")
        self.profiles = [Profile(index + 1, path) for index, path in enumerate(files)]
        self.stopping = threading.Event()

    def start_all(self) -> None:
        for profile in self.profiles:
            try:
                profile.start()
                profile.wait_ready()
            except Exception as error:
                log("profile_start_failed", profile=profile.name, error=str(error)[:200])

    def stop_all(self) -> None:
        for profile in reversed(self.profiles):
            profile.stop()

    def health(self) -> dict[str, object]:
        profiles = [profile.health() for profile in self.profiles]
        return {"ok": all(profile.get("warp") == "on" for profile in profiles),
                "profiles": profiles}

    def rotate(self) -> dict[str, object]:
        with self.lock:
            log("rotate_started", profiles=len(self.profiles))
            results = []
            for profile in self.profiles:
                old_ip = profile.ip
                final_ip = None
                error = ""
                for attempt in range(1, ROTATE_ATTEMPTS + 1):
                    profile.stop()
                    try:
                        profile.start()
                        info = profile.wait_ready()
                        final_ip = info.get("ip")
                        if not old_ip or final_ip != old_ip:
                            break
                        error = "egress address unchanged"
                    except Exception as failure:
                        error = str(failure)[:200]
                    log("rotate_retry", profile=profile.name, attempt=attempt,
                        error=error)
                results.append({
                    "name": profile.name,
                    "ok": bool(final_ip),
                    "before": old_ip,
                    "after": final_ip,
                    "changed": bool(old_ip and final_ip and old_ip != final_ip),
                    "error": error or None,
                })
            changed = any(item["changed"] for item in results)
            ok = all(item["ok"] for item in results)
            log("rotate_finished", ok=ok, changed=changed,
                result=[{"name": item["name"], "changed": item["changed"]} for item in results])
            return {"ok": ok, "changed": changed, "profiles": results}

    def background_health(self) -> None:
        while not self.stopping.wait(30):
            for profile in self.profiles:
                if profile.proc is None or profile.proc.poll() is not None:
                    try:
                        profile.start()
                        profile.wait_ready()
                    except Exception as error:
                        log("health_restart_failed", profile=profile.name, error=str(error)[:200])
                    continue
                try:
                    info = trace_via(profile.port, timeout=8)
                    if info.get("warp") != "on":
                        raise RuntimeError(f"trace={info}")
                    profile.ip = info.get("ip") or profile.ip
                except Exception as error:
                    log("health_restart", profile=profile.name, error=str(error)[:160])
                    try:
                        profile.stop()
                        profile.start()
                        profile.wait_ready()
                    except Exception as restart_error:
                        log("health_restart_failed", profile=profile.name,
                            error=str(restart_error)[:160])


class ControlHandler(BaseHTTPRequestHandler):
    pool: Pool  # assigned before the server starts

    def log_message(self, _format: str, *_args: object) -> None:
        return

    def send_json(self, status: int, payload: dict[str, object]) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            # A short health probe may time out while WireGuard is starting.
            # It is not a reason to emit a traceback into the journal.
            pass

    def do_GET(self) -> None:  # noqa: N802 - stdlib API
        if self.path == "/health":
            self.send_json(200, self.pool.health())
        else:
            self.send_json(404, {"ok": False, "error": "not found"})

    def do_POST(self) -> None:  # noqa: N802 - stdlib API
        if self.path != "/rotate":
            self.send_json(404, {"ok": False, "error": "not found"})
            return
        try:
            self.send_json(200, self.pool.rotate())
        except Exception as error:
            log("rotate_failed", error=str(error)[:240])
            self.send_json(500, {"ok": False, "error": str(error)[:240]})


def main() -> int:
    try:
        pool = Pool()
    except Exception as error:
        log("startup_failed", error=str(error))
        return 1
    pool.start_all()

    def shutdown(_signum: int, _frame: object) -> None:
        pool.stopping.set()
        pool.stop_all()
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)

    health = threading.Thread(target=pool.background_health, name="warp-pool-health", daemon=True)
    health.start()

    ControlHandler.pool = pool
    server = ThreadingHTTPServer((CONTROL_HOST, CONTROL_PORT), ControlHandler)
    server.daemon_threads = True
    log("listening", control=f"{CONTROL_HOST}:{CONTROL_PORT}",
        profiles=[{"name": p.name, "port": p.port, "ip": p.ip} for p in pool.profiles])
    try:
        server.serve_forever(poll_interval=1.0)
    finally:
        server.server_close()
        pool.stopping.set()
        pool.stop_all()
    return 0


if __name__ == "__main__":
    sys.exit(main())
