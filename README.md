# fastscan

Fast TCP connect scanner (non-root). This is **not** a masscan replacement (no raw SYN); it uses regular `connect()` so it works without privileges.

## Build

```bash
go build -o fastscan .
```

## Run

Scan a single host:

```bash
./fastscan -targets 192.168.1.1 -ports 22,53,80,443 -workers 256 -timeout 150ms -retries 0 -max-inflight-per-host 256 -stream-open
```

Scan a subnet:

```bash
./fastscan -targets 192.168.1.0/24 -ports 1-1024 -workers 4096 -timeout 150ms -retries 0 -max-inflight-per-host 512 -stream-open
```

Notes:
- `timeout` strongly affects speed when ports are filtered/dropped (no RST).
- `max-inflight-per-host` is a safety valve to avoid melting one host and to reduce false negatives.
- Progress bar prints to `stderr` (doesn't corrupt `open ...` lines).

