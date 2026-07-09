#!/usr/bin/env python3
"""Minimal NetBox API stub for end-to-end tests.

Serves the slice of the NetBox REST API that the data source touches during e2e:
the API root/app indexes (for object-type discovery), a devices list (for object
and count queries), the changelog (for annotations), and /api/status/ (health).
Responses mirror pkg/provider/netbox/netbox_test.go's mockNetBox so the stub and
the Go unit tests stay in sync.

URLs embedded in index responses are built from the request Host header, so the
stub works unchanged whether reached as localhost or as a Docker Compose service
name (e.g. http://netbox-stub:8080).

No external dependencies — standard library only. Configure the port with PORT
(default 8080).
"""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

# A devices list whose envelope "count" (500) intentionally exceeds the number of
# returned rows (2), so count queries exercise the envelope-total path.
DEVICES = {
    "count": 500,
    "next": None,
    "results": [
        {
            "id": 1,
            "name": "leaf1",
            "site": {"id": 2, "name": "dc1", "slug": "dc1"},
            "status": {"value": "active", "label": "Active"},
            "interface_count": 48,
        },
        {
            "id": 2,
            "name": "leaf2",
            "site": {"id": 2, "name": "dc1", "slug": "dc1"},
            "status": {"value": "offline", "label": "Offline"},
            "interface_count": 24,
        },
    ],
}

CHANGES = {
    "count": 1,
    "next": None,
    "results": [
        {
            "time": "2026-06-27T00:42:48Z",
            "user_name": "admin",
            "action": {"value": "update", "label": "Updated"},
            "changed_object_type": "dcim.device",
            "object_repr": "leaf1",
        }
    ],
}

EMPTY_LIST = {"count": 0, "next": None, "results": []}


def _index(base):
    return {
        "dcim": f"{base}/api/dcim/",
        "ipam": f"{base}/api/ipam/",
        "plugins": f"{base}/api/plugins/",
        "status": f"{base}/api/status/",
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # keep CI logs quiet
        pass

    def _send(self, obj, status=200):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        base = f"http://{self.headers.get('Host', 'localhost')}"
        path = urlsplit(self.path).path
        if not path.endswith("/"):
            path += "/"

        if path == "/api/":
            return self._send(_index(base))
        if path == "/api/dcim/":
            return self._send(
                {
                    "devices": f"{base}/api/dcim/devices/",
                    "interfaces": f"{base}/api/dcim/interfaces/",
                }
            )
        if path == "/api/ipam/":
            return self._send({"ip-addresses": f"{base}/api/ipam/ip-addresses/"})
        if path == "/api/plugins/":
            return self._send(
                {
                    "bgp": f"{base}/api/plugins/bgp/",
                    "installed-plugins": f"{base}/api/plugins/installed-plugins/",
                }
            )
        if path == "/api/plugins/bgp/":
            return self._send({"bgp-sessions": f"{base}/api/plugins/bgp/bgp-sessions/"})
        if path == "/api/plugins/installed-plugins/":
            return self._send({"count": 1, "next": None, "results": [{"name": "bgp"}]})
        if path == "/api/status/":
            if not self.headers.get("Authorization"):
                return self._send({"detail": "auth required"}, status=403)
            return self._send({"netbox-version": "4.6.0"})
        if path == "/api/dcim/devices/":
            return self._send(DEVICES)
        if path == "/api/core/object-changes/":
            return self._send(CHANGES)
        # Any other list endpoint: an empty, well-formed page.
        return self._send(EMPTY_LIST)


def main():
    port = int(os.environ.get("PORT", "8080"))
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
