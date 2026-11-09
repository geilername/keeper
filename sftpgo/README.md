# SFTPGo

Off-the-shelf FTP endpoint for the camera (`drakkan/sftpgo:v2.7.6`). Every
setting is in this directory and is applied at startup. There is no web
admin and nothing to click.

| File | What it does |
|---|---|
| `sftpgo.env` | Server config as `SFTPGO_*` env vars: FTP on 21 only, passive ports 50000–50009, umask 022, in-memory data provider |
| `initial-data.json` | User `camera` (home `/spool/incoming`), event action `ingest-hook`, event rule `upload-to-ingest` (SFTPGo dump format, version 17) |
| `entrypoint.sh` | Puts `CAMERA_FTP_PASSWORD` (default `camera`) into the dump, then runs `sftpgo serve` |

## Upload hook contract

On every **successful** upload by `camera`, SFTPGo sends this request
**synchronously**, before it answers the FTP client:

```
POST http://ingest:8080/hooks/sftpgo
Content-Type: application/json

{"event":"upload","fs_path":"/spool/incoming/beach-dog-01.jpg","virtual_path":"/beach-dog-01.jpg","file_size":328707,"user":"camera","timestamp":"1794251715090216783"}
```

| Field | Placeholder | Type | Notes |
|---|---|---|---|
| `event` | `{{.Event}}` | string | always `upload` (the rule only fires on upload) |
| `fs_path` | `{{.FsPath}}` | string | absolute path in the shared `spool` volume |
| `virtual_path` | `{{.VirtualPath}}` | string | path as the camera sees it, starts with `/` |
| `file_size` | `{{.FileSize}}` | number | bytes |
| `user` | `{{.Name}}` | string | SFTPGo username, `camera` |
| `timestamp` | `{{.Timestamp}}` | string of digits | Unix time in **nanoseconds**, when the upload finished (a string, so it survives float64 JSON parsers) |

SFTPGo JSON-escapes the values because the action sets
`Content-Type: application/json`. A file named `we"ird.jpg` arrives as
`"we\"ird.jpg"`.

Status codes (checked against v2.7.6):

- **200–204**: success. The FTP client gets `226` and the file stays in
  `/spool/incoming`.
- **Anything else, a timeout (20 s) or a connection error**: SFTPGo
  **deletes the uploaded file** and fails the transfer with
  `550 Issue during transfer`. SFTPGo does not retry the hook itself. The
  camera has to send the file again (camera-sim retries each frame 3 times).
  So ingest must only answer 2xx once it no longer needs anything from
  SFTPGo, and must answer 5xx when the camera should re-send.
- Failed or aborted transfers (`event_statuses: [1]`) never call ingest.

## Files on the spool

SFTPGo runs as uid/gid 1000 (the image default) with umask 022, so uploads
are `1000:1000 0644`. `setstat_mode=1` ignores client chmod/chown/mtime
requests. `/spool/incoming` must exist and be owned by 1000 (the `spool-init`
service).

## Port 21 as uid 1000

Docker lets containers bind ports below 1024 by default. Podman and some
other runtimes do not, so compose sets
`net.ipv4.ip_unprivileged_port_start: 21` for this container only.

## A real camera (stretch)

To reach SFTPGo from the LAN, publish `21` and `50000-50009`, and set
`SFTPGO_FTPD__BINDINGS__0__FORCE_PASSIVE_IP` to the host's LAN IP.
Otherwise SFTPGo advertises its container IP in PASV replies. FTPS with a
real certificate is separate stretch work (HACKATHON-PLAN.md §9).
