# fastscan

Fast TCP connect scanner (non-root). This is **not** a masscan replacement (no raw SYN); it uses regular `connect()` so it works without privileges.

## Binaries

Prebuilt binaries are committed under `bin/`:

```bash
bin/fastscan_debian12_amd64
bin/fastscan_debian12_arm64
bin/fastscan_amzn2023_aarch64
```

## Build

```bash
go build -o fastscan .
```

## Run

Scan a single host:

```bash
./fastscan -targets 192.168.1.1 -ports 22,53,80,443 -workers 256 -timeout 150ms -retries 0 -max-inflight-per-host 256 -stream-open
```

Scan a single host with a port range:

```bash
./fastscan -targets 192.168.1.1 -ports 80-1000 -workers 512 -timeout 150ms -retries 0 -max-inflight-per-host 512 -stream-open
```

Scan a subnet:

```bash
./fastscan -targets 192.168.1.0/24 -ports 1-1024 -workers 4096 -timeout 150ms -retries 0 -max-inflight-per-host 512 -stream-open
```

Notes:
- `timeout` strongly affects speed when ports are filtered/dropped (no RST).
- `max-inflight-per-host` is a safety valve to avoid melting one host and to reduce false negatives.
- Progress bar prints to `stderr` (doesn't corrupt `open ...` lines).

## Flags

All flags (run `./fastscan -h` for the authoritative list):

- `-targets` (string, required): IP/CIDR/list, e.g. `192.168.1.10,10.0.0.0/24`
- `-ports` (string, default `80,443`): ports list/ranges, e.g. `80,443,1-1024`
- `-workers` (int, default `8192`): total concurrent workers (global parallelism)
- `-timeout` (duration, default `300ms`): per-dial timeout; lower is faster on filtered ports but can miss slow networks
- `-retries` (int, default `2`): retries for timeout/resource errors (set `0` for max speed)
- `-retry-backoff` (duration, default `20ms`): base backoff between retries (exponential per attempt)
- `-max-inflight-per-host` (int, default `256`): max concurrent dials to one host (`0` = unlimited)
- `-queue` (int, default `262144`): internal jobs queue size
- `-stream-open` (bool, default `false`): print `open ip:port` immediately (to `stdout`)
- `-show-closed-errors` (bool, default `false`): print dial errors (very noisy)
- `-err-print-limit` (int, default `50`): max dial errors to print when `-show-closed-errors` is set (`0` = unlimited)
- `-progress` (bool, default `true`): show progress bar
- `-progress-interval` (duration, default `1s`): progress refresh rate
- `-progress-style` (string, default `auto`): `auto|cr|line`
- `-tune-socket` (bool, default `false`): enable socket tuning (may reduce compatibility)

Output behavior:
- `open ...` lines go to `stdout`.
- Progress goes to `stderr` and should not corrupt `open` lines.
- A final summary prints totals and an error breakdown.
