# fastscan

Fast, dependency-free **TCP connect** port scanner. A single static binary — drop it on a host and scan. No root, no libraries, no shell required.

`fastscan` opens an ordinary TCP connection (`connect()`) to each `ip:port` and reports the ones that accept. Because it uses the normal socket API instead of crafting raw SYN packets, it needs **no privileges** and runs anywhere a Go binary runs. It is **not** a masscan replacement for internet-wide raw-SYN sweeps — it trades peak packet rate for zero-privilege portability and simplicity.

<img width="1698" height="396" alt="image" src="https://github.com/user-attachments/assets/b139f046-7d60-4157-b420-6e0fae88c27a" />

---

## What it does

- Takes a set of **targets** (single IPs, IPv4 ranges, CIDRs, or a file) and a set of **ports** (lists and ranges).
- Fans the resulting `ip:port` pairs across a large pool of worker goroutines, each doing a `connect()` with a timeout.
- Reports every port that accepted the connection as `open ip:port` on `stdout`.
- Prints a live progress bar with rate/ETA to `stderr`, and a final summary with a per-class error breakdown (timeout / refused / unreachable / …).
- Can optionally detach and run in the **background** (`-daemon`), writing results to a file and returning your shell immediately.

Targets are IP-based only — `192.0.2.10`, `10.0.0.0/24`, `100.64.0.0-100.127.255.255`. Hostnames/DNS names are not resolved.

## Why fastscan

- **Zero privileges.** Plain `connect()` scanning — no `CAP_NET_RAW`, no root, works inside containers and locked-down environments.
- **Zero dependencies.** One statically linked binary. No libc, no Python, no `nmap`, no shell. `scp` it over and run.
- **Portable.** The same `amd64` binary runs on Debian 12 *and* CentOS/RHEL 7 (kernel 3.10); the `arm64` binary runs on Amazon Linux 2023 and any other arm64 Linux. See [the support matrix](#prebuilt-binaries).
- **Fast.** Tens of thousands of concurrent dials with a tunable worker pool; throughput is bounded mainly by your timeout, file-descriptor limit, and the network — not by the tool.
- **Accurate-ish.** Configurable retries with exponential backoff reduce false negatives caused by transient drops / resource pressure on lossy networks.
- **Polite.** A per-host in-flight cap avoids hammering a single host into dropping packets (which would otherwise show up as false negatives).
- **Operable.** Live progress + ETA, sorted or streamed output, clean `stdout`/`stderr` separation, and a background mode with PID/output/log files.

## How it works

```
targets ──▶ job feeder ──▶ [ jobs channel ] ──▶ N workers ──▶ connect() ──▶ open?─▶ stdout
(IP/CIDR/range/file)          (port-major)        │                          └─ classify error ─▶ counters
ports  ──────────────────────────────────────────┘                          progress/ETA ─────▶ stderr
```

- **Port-major scheduling.** Jobs are emitted port-by-port across all hosts (`for each port: for each host`), so consecutive dials hit *different* hosts. This spreads load and reduces packet loss from bursting a single host.
- **Worker pool.** `-workers` goroutines each pull jobs and dial with a per-attempt timeout. This is the global parallelism knob.
- **Per-host in-flight cap.** `-max-inflight-per-host` limits concurrent dials to any one host (a token channel per host). Automatically disabled for very large target sets (> 100k hosts) to save memory.
- **Retries with backoff.** On *timeout* or *resource* errors (`EMFILE`, `ENOBUFS`, `EADDRNOTAVAIL`, …) a dial is retried up to `-retries` times. Both the per-attempt timeout and the backoff grow exponentially per attempt. Connection-refused is **not** retried (it is a definitive "closed").
- **Output.** Open ports print as `open ip:port`. Without `-stream-open` they are buffered and printed **sorted** at the end; with `-stream-open` they print **immediately** as found. Progress and notes always go to `stderr`, so `stdout` stays a clean list of results.

## Performance

Each `(ip, port)` is one `connect()`:

| Port state | What happens | Cost |
|---|---|---|
| **open** | handshake completes | ~1 RTT (fast) |
| **closed** | kernel returns RST → "connection refused" | ~1 RTT (fast) |
| **filtered** | packet dropped, no reply | blocks for the **full `-timeout`** (then retries) |

So the slow case is **filtered** ports. Rough throughput ceiling:

```
probes/sec ≈ workers / (timeout × retry_factor)
```

e.g. `-workers 8192 -timeout 300ms -retries 0` on all-filtered ports ≈ `8192 / 0.3` ≈ **~27k probes/s** — *if* nothing else is the bottleneck. On networks that return RST/handshake quickly (most LANs), real throughput is far higher because dials finish in ~1 RTT instead of waiting out the timeout.

**Real bottlenecks**, in rough order:

- **Open file descriptors.** Each in-flight dial uses an fd. Keep `ulimit -n` comfortably above `-workers` (e.g. `ulimit -n 1048576`). The scanner retries `EMFILE`/`ENFILE`/`ENOBUFS`, but you'll lose speed.
- **Ephemeral ports.** ~28k source ports per destination tuple; scanning many ports on one host very fast can exhaust them (shows up as `addr_not_avail`).
- **Network pps / bandwidth**, and any **rate-limiting / SYN throttling** on the path or target.

**Tuning cheatsheet**

- *Fastest:* low `-timeout` (e.g. `100–150ms`) + `-retries 0`. Best on fast/reliable networks; risks false negatives on slow/lossy ones.
- *More thorough:* higher `-timeout` + `-retries 2`. Catches slow hosts at the cost of speed.
- *Scale up:* raise `-workers`, but raise `ulimit -n` to match.
- *Be gentle to a single host:* keep `-max-inflight-per-host` modest (default 256).

The final summary reports the achieved `avg conn/s`, and the live bar shows current rate + ETA.

## Prebuilt binaries

Statically linked, stripped binaries are committed under `bin/` (verify with `bin/SHASUMS256.txt`):

| Binary | Built for | Also runs on | GOOS/GOARCH |
|---|---|---|---|
| `fastscan_amzn2023_aarch64` | Amazon Linux 2023 (`6.12.x`, aarch64) | any arm64 Linux | `linux/arm64` |
| `fastscan_debian12_amd64` | Debian 12 (`6.1.x`, x86_64) | any modern amd64 Linux | `linux/amd64` |
| `fastscan_el7_amd64` | CentOS/RHEL 7 (`3.10.x`, x86_64) | any amd64 Linux, kernel ≥ 2.6.32 | `linux/amd64` |

Because the binaries are fully static (`CGO_ENABLED=0`), a binary is tied to its **architecture and minimum kernel only**, not to a distro or libc version. The `debian12_amd64` and `el7_amd64` files are therefore **byte-for-byte identical** — one amd64 binary already covers Debian 12, CentOS/RHEL 7, and everything in between; the two names are kept only for clarity. The arm64 binary likewise runs on any arm64 Linux.

```bash
# verify and run
cd bin && sha256sum -c SHASUMS256.txt        # (or: shasum -a 256 -c SHASUMS256.txt)
chmod +x fastscan_debian12_amd64
./fastscan_debian12_amd64 -targets 192.168.1.1 -ports 1-1024 -stream-open
```

## Build from source

Requires Go 1.25+ (see `go.mod`). No other dependencies.

```bash
# Native build for the current host
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o fastscan .
```

Cross-compiling needs **no external toolchain** as long as `CGO_ENABLED=0` (Go ships every target). The flags matter:

- `CGO_ENABLED=0` — produce a **fully static** binary with the pure-Go networking stack. This is what lets one binary run on old glibc (CentOS 7) and any distro. Leaving CGO on would link the build host's libc and break on older systems.
- `-trimpath` — strip local filesystem paths → reproducible builds.
- `-ldflags="-s -w"` — drop the symbol table and DWARF → smaller binary.

Build the three shipped targets:

```bash
# Amazon Linux 2023 / any arm64 Linux
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/fastscan_amzn2023_aarch64 .

# Debian 12 / any modern amd64 Linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/fastscan_debian12_amd64 .

# CentOS/RHEL 7 (el7, kernel 3.10) / any amd64 Linux — identical to the Debian 12 amd64 binary
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/fastscan_el7_amd64 .

# refresh checksums
( cd bin && shasum -a 256 fastscan_* > SHASUMS256.txt )
```

Verify a build is truly static:

```bash
file bin/fastscan_el7_amd64     # → "ELF 64-bit ... statically linked ... stripped"
ldd  bin/fastscan_el7_amd64     # → "not a dynamic executable"  (run on a Linux host)
```

## Usage

```
fastscan -targets <spec> [-targets-file <path>] -ports <spec> [options]
```

Run `./fastscan -h` for the authoritative list. Flags by group:

**Targets & ports**
- `-targets` (string): IP / CIDR / range / comma-list, e.g. `192.168.1.10,10.0.0.0/24,100.64.0.0-100.64.0.255`. Also accepts `@path` to read a file. Either this or `-targets-file` is required.
- `-targets-file` (string): file with one target per line; supports `#` comments (whole-line and inline).
- `-ports` (string, default `80,443`): ports list/ranges, e.g. `22,80,443,8000-8100`. De-duplicated, clamped to `1–65535`.

**Performance**
- `-workers` (int, default `8192`): total concurrent dials (global parallelism).
- `-timeout` (duration, default `300ms`): per-attempt dial timeout. Lower = faster on filtered ports, but can miss slow networks.
- `-max-inflight-per-host` (int, default `256`): cap concurrent dials to one host (`0` = unlimited). Auto-disabled above 100k targets.
- `-queue` (int, default `262144`): internal job-queue size.

**Reliability**
- `-retries` (int, default `2`): retries on timeout/resource errors (`0` = max speed). Refused is never retried.
- `-retry-backoff` (duration, default `20ms`): base backoff between retries (grows exponentially per attempt).
- `-tune-socket` (bool, default `false`): aggressive socket tuning (`TCP_NODELAY`, small buffers); may reduce compatibility.

**Output**
- `-stream-open` (bool, default `false`): print `open ip:port` immediately instead of buffering + sorting at the end.
- `-show-closed-errors` (bool, default `false`): print individual dial errors (very noisy).
- `-err-print-limit` (int, default `50`): cap on printed dial errors when `-show-closed-errors` is set (`0` = unlimited).

**Progress**
- `-progress` (bool, default `true`): live progress bar on `stderr`.
- `-progress-interval` (duration, default `1s`): refresh rate.
- `-progress-style` (string, default `auto`): `auto` (CR in a TTY, line otherwise) / `cr` / `line`.

**Background**
- `-daemon` (bool, default `false`): detach and run in the background — own session via `setsid`, no controlling terminal; the shell returns immediately. No shell/`nohup` dependency: the binary re-execs itself.
- `-output` (string): write results to a file instead of `stdout` (default in `-daemon`: `fastscan.out`).
- `-log` (string): write progress/summary to a file instead of `stderr` (default in `-daemon`: `<output>.log`).
- `-pid-file` (string): in `-daemon` mode, also write the background PID here.

## Examples

Single host, a few ports, streaming:

```bash
./fastscan -targets 192.168.1.1 -ports 22,53,80,443 -workers 256 -timeout 150ms -retries 0 -stream-open
```

Single host, full port range:

```bash
./fastscan -targets 192.168.1.1 -ports 1-65535 -workers 2048 -timeout 200ms -retries 1 -stream-open
```

A /24 subnet, common ports, sorted output to a file:

```bash
./fastscan -targets 192.168.1.0/24 -ports 1-1024 -workers 4096 -timeout 150ms -retries 0 > open_lan.txt
```

A large IPv4 range on one port (CIDR or `a-b` form are equivalent):

```bash
./fastscan -targets 100.64.0.0/10        -ports 9000 -workers 4096 -timeout 150ms -retries 0 -stream-open > open_9000.txt
./fastscan -targets 100.64.0.0-100.127.255.255 -ports 9000 -workers 4096 -timeout 150ms -retries 0 -stream-open > open_9000.txt
```

Targets from a file (one per line, `#` comments allowed):

```bash
cat > targets.txt <<'EOF'
# corp ranges
192.168.1.1
192.168.10.0/24        # office subnet
10.0.0.0-10.0.3.255    # dmz
EOF

./fastscan -targets-file targets.txt -ports 80-1000 -workers 4096 -timeout 150ms -retries 0 -stream-open
```

Run detached in the background (no shell or `nohup` needed — the binary re-execs itself with `setsid`):

```bash
./fastscan -daemon -targets 100.64.0.0/10 -ports 9000 -workers 4096 -timeout 150ms -retries 0 \
           -output open_9000.txt -pid-file scan.pid
# prints pid + file paths and returns immediately:
#   results       -> open_9000.txt
#   live progress -> open_9000.txt.log     (tail -f it to watch)
# stop early with:  kill $(cat scan.pid)
```

Extract just the open IPs from results:

```bash
grep '^open ' open_9000.txt | awk '{print $2}' | cut -d: -f1 | sort -u
```

## Output format

- `open ip:port` lines → **stdout** (sorted at the end, or streamed with `-stream-open`).
- A header line and a final summary → stdout:
  ```
  targets=256 ports=1024 total=262144 workers=4096 timeout=150ms retries=0 per_host=256
  done scanned=262144 open=37 elapsed=12.480s avg=21005 conn/s
  errors timeout=261800 refused=307 addr_not_avail=0 unreach=0 perm=0 other=0
  ```
- Progress bar + notes → **stderr** (won't corrupt the `open` list on stdout).
- Error classes: `timeout` (filtered/slow), `refused` (closed), `addr_not_avail` (ephemeral-port pressure), `unreach` (no route / host down), `perm` (firewall/sandbox blocking connect), `other`.

## Notes & caveats

- **Filtered vs closed:** `refused` means the port is closed (host reachable). `timeout` usually means filtered/dropped — these are the ones that cost a full `-timeout` each.
- **`perm` errors:** a burst of "operation not permitted" usually means a host firewall, sandbox, or egress policy is blocking outbound `connect()`.
- **IPv6:** single IPv6 *addresses* work; IPv6 *CIDRs* are rejected on purpose (too large to expand). IPv4 CIDRs drop the network/broadcast address for prefixes with ≥ 4 hosts.
- **ulimit:** for high `-workers`, raise the open-file limit (`ulimit -n 1048576`).
- **Authorization:** only scan hosts and networks you are authorized to test.
```
