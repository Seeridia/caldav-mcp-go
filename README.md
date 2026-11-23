# caldav-mcp-go

A Go MCP server for one CalDAV account. It provides calendar discovery, event search and editing, display reminders, and availability across selected calendars.

## Run locally

Requires Go 1.27 or later and an existing CalDAV account with at least one calendar.

```sh
export CALDAV_URL='https://calendar.example.com/'
export CALDAV_USERNAME='your-account'
export TZ='Asia/Shanghai'
# Set CALDAV_PASSWORD and MCP_TOKEN through your shell or secret manager.
go run ./cmd/caldav-mcp
```

Connect an MCP client using Streamable HTTP at `http://127.0.0.1:8080/mcp` with the header `Authorization: Bearer <MCP_TOKEN>`. Set `LISTEN_ADDR` to change the binding. For remote access, put the endpoint behind an HTTPS reverse proxy. `GET /healthz` reports process liveness.

## Tools

| Tool | Operation |
| --- | --- |
| `list_calendars` | Discover event calendars and their IDs |
| `list_events` | List occurrences in a time range, optionally filtering title, location, and description |
| `get_event` | Read an event or recurring series master |
| `create_event` | Create a timed or all-day event with optional display reminders |
| `update_event` | Change selected fields of an event or series master |
| `delete_event` | Delete an event or full series by UID |
| `find_available_slots` | Find openings of a given length across selected calendars |

Use the calendar ID returned by `list_calendars` for subsequent calls. Timed inputs use RFC3339 with an offset and whole-second precision. Dates use `YYYY-MM-DD`, and all-day ends are exclusive. An omitted event end defaults to one hour for timed events or one calendar day for all-day events. Timed writes are stored in UTC.

For example, this creates a one-day event that does not block scheduling and has two reminders:

```json
{
  "calendar_id": "/account/personal/",
  "summary": "Travel check-in",
  "start": "2026-11-25",
  "busy": false,
  "reminders": [10, 30]
}
```

Reminders are whole minutes before start. Zero means at start. In an update, an omitted list preserves existing reminders, while an empty list clears simple relative-to-start display reminders. Other alarm types are preserved. Notifications are shown by the calendar client.

Availability defaults to 09:00–17:00 in `TZ` (UTC unless set). Override the window or time zone in the tool call, or limit the search to weekdays. When any requested calendar cannot be read, the result marks it as unavailable and reports no confirmed openings.

Recurring occurrences are expanded by the CalDAV server. A series UID targets the master for details and updates and the entire series for deletion. Changing the time of recurring events and editing individual instances are outside this release's tool surface. Updates and deletes require a strong ETag from the CalDAV server.

## Container deployment

The container listens on port 8080. Provide the same CalDAV settings and
`MCP_TOKEN` as above. The built-in Docker health check uses `/healthz`.

On `main`, GitHub Actions tests the code, publishes a commit-tagged Linux AMD64
image to Tencent Cloud Registry, and records the resulting digest in GitHub
under the `caldav-production-image` deployment environment. That record signals
that an image is available. Server-shanghai polls the record and uses its local
Dokploy connection to update and deploy the production Raw Compose by digest.
The `caldav-image-deploy.timer` service logs the actual deployment result on the
host. For rollback, stop the timer during recovery and restore a known-good
digest in Dokploy before redeploying.
