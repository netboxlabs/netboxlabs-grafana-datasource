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
import ipaddress
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

# A devices list whose envelope "count" matches the rows served, as a real NetBox
# does. It must stay consistent: the plugin treats rows < count as a truncated
# result, which is a hard error for alertTable queries (see resultNotices /
# truncationError in pkg/plugin/notices.go). The envelope-total path for count
# queries is covered by unit tests (TestQuery_Count_NeverErrorsOnLargeTotal),
# which assert a large total with zero rows far more directly than this stub can.
DEVICES = {
    "count": 2,
    "next": None,
    "results": [
        {
            "id": 1,
            "name": "leaf1",
            "site": {"id": 2, "name": "dc1", "slug": "dc1"},
            "status": {"value": "active", "label": "Active"},
            "primary_ip4": {"id": 101, "address": "10.0.0.1/24"},
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

# Address records for the ip-enrichment scope path: two addresses of leaf1 (one
# its primary IP, one not) in 10.0.0.0/24, one unassigned address there, and
# one address outside it. The device hop reads DEVICES by id; leaf1 (id 1) is
# given a primary_ip4 above so is_primary_ip has something to answer.
IP_ADDRESSES = [
    {"id": 101, "address": "10.0.0.1/24", "status": {"value": "active", "label": "Active"},
     "assigned_object_type": "dcim.interface", "assigned_object_id": 11,
     "assigned_object": {"id": 11, "name": "Ethernet1", "device": {"id": 1, "name": "leaf1"}}},
    {"id": 102, "address": "10.0.0.2/24", "status": {"value": "active", "label": "Active"},
     "assigned_object_type": "dcim.interface", "assigned_object_id": 12,
     "assigned_object": {"id": 12, "name": "Loopback0", "device": {"id": 1, "name": "leaf1"}}},
    {"id": 103, "address": "10.0.0.9/24", "status": {"value": "reserved", "label": "Reserved"},
     "assigned_object_type": None, "assigned_object_id": None, "assigned_object": None},
    {"id": 104, "address": "10.9.9.9/24", "status": {"value": "active", "label": "Active"},
     "assigned_object_type": None, "assigned_object_id": None, "assigned_object": None},
]


def _in_parent(address, parent):
    try:
        return ipaddress.ip_interface(address).ip in ipaddress.ip_network(parent, strict=False)
    except ValueError:
        # NetBox answers a malformed parent with HTTP 400; matching nothing is
        # the closest a stub with one status code gets, and it keeps the
        # handler thread alive.
        return False


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
            # Honors the site filter (by slug, repeated params are OR) as NetBox
            # does, so a spec can tell a dropped filter from one that was sent and
            # matched nothing. The count follows the rows for the reason above.
            sites = parse_qs(urlsplit(self.path).query).get("site")
            if sites:
                rows = [d for d in DEVICES["results"] if d["site"]["slug"] in sites]
                return self._send({"count": len(rows), "next": None, "results": rows})
            return self._send(DEVICES)
        if path == "/api/ipam/ip-addresses/":
            # parent= (prefix scope) and address= (the list path's lookup),
            # repeated params OR'd, as NetBox does. Count follows the rows.
            q = parse_qs(urlsplit(self.path).query)
            rows = IP_ADDRESSES
            if q.get("parent"):
                rows = [a for a in rows if any(_in_parent(a["address"], p) for p in q["parent"])]
            if q.get("address"):
                wanted = {x.split("/")[0] for x in q["address"]}
                rows = [a for a in rows if a["address"].split("/")[0] in wanted]
            return self._send({"count": len(rows), "next": None, "results": rows})
        if path == "/api/core/object-changes/":
            return self._send(CHANGES)
        # Any other list endpoint: an empty, well-formed page.
        return self._send(EMPTY_LIST)


def main():
    port = int(os.environ.get("PORT", "8080"))
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
