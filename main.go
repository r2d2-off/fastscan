package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type targetPort struct {
	ip   string
	port int
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	targetsFlag := flag.String("targets", "", "IP/CIDR/list: 192.168.1.10,10.0.0.0/24")
	targetsFileFlag := flag.String("targets-file", "", "path to file with targets (one per line; supports comments with #)")
	portsFlag := flag.String("ports", "80,443", "ports: 80,443,1-1024")
	workersFlag := flag.Int("workers", 8192, "concurrent workers")
	perHostInflightFlag := flag.Int("max-inflight-per-host", 256, "max concurrent dials to one host (0 = unlimited)")
	timeoutFlag := flag.Duration("timeout", 300*time.Millisecond, "dial timeout")
	retriesFlag := flag.Int("retries", 2, "retries for timeout/resource errors")
	retryBackoffFlag := flag.Duration("retry-backoff", 20*time.Millisecond, "base backoff between retries")
	tuneSocketFlag := flag.Bool("tune-socket", false, "enable aggressive socket tuning (can reduce compatibility)")
	streamOpenFlag := flag.Bool("stream-open", false, "print open results immediately")
	errPrintLimitFlag := flag.Int("err-print-limit", 50, "max dial errors to print when -show-closed-errors is set")
	progressFlag := flag.Bool("progress", true, "print progress to stderr")
	progressIntervalFlag := flag.Duration("progress-interval", 1*time.Second, "progress update interval")
	progressStyleFlag := flag.String("progress-style", "auto", "auto|cr|line (cr uses \\r, line prints new lines)")
	queueFlag := flag.Int("queue", 262144, "job queue size")
	showClosedFlag := flag.Bool("show-closed-errors", false, "print dial errors (very noisy)")
	flag.Parse()

	if *targetsFlag == "" && *targetsFileFlag == "" {
		exitf("use -targets and/or -targets-file, example: -targets 10.0.0.0/24 -ports 1-1024")
	}
	if *workersFlag <= 0 {
		exitf("workers must be > 0")
	}
	if *queueFlag <= 0 {
		exitf("queue must be > 0")
	}
	if *perHostInflightFlag < 0 {
		exitf("max-inflight-per-host must be >= 0")
	}
	if *retriesFlag < 0 {
		exitf("retries must be >= 0")
	}
	if *errPrintLimitFlag < 0 {
		exitf("err-print-limit must be >= 0")
	}
	if *progressIntervalFlag <= 0 {
		exitf("progress-interval must be > 0")
	}

	ports, err := parsePorts(*portsFlag)
	if err != nil {
		exitf("ports parse error: %v", err)
	}
	if len(ports) == 0 {
		exitf("no valid ports")
	}

	targetInputs, err := gatherTargetInputs(*targetsFlag, *targetsFileFlag)
	if err != nil {
		exitf("targets input error: %v", err)
	}
	targets, err := parseTargets(targetInputs)
	if err != nil {
		exitf("targets parse error: %v", err)
	}
	targetCount := targets.Count()
	if targetCount == 0 {
		exitf("no targets after parsing")
	}

	totalTasks := targetCount * uint64(len(ports))

	perHostInflight := *perHostInflightFlag
	// Per-host limiter stores state per host. Disable it for very large target sets.
	if perHostInflight > 0 && targetCount > 100_000 {
		fmt.Fprintf(os.Stderr, "note: disabling -max-inflight-per-host for large target set (targets=%d)\n", targetCount)
		perHostInflight = 0
	}

	fmt.Printf("targets=%d ports=%d total=%d workers=%d timeout=%s retries=%d per_host=%d\n",
		targetCount, len(ports), totalTasks, *workersFlag, timeoutFlag.String(), *retriesFlag, perHostInflight)

	start := time.Now()
	ctx := context.Background()
	jobs := make(chan targetPort, *queueFlag)
	openResults := make(chan string, 4096)
	errResults := make(chan string, 1024)

	var scanned atomic.Uint64
	var openCount atomic.Uint64
	var errTimeout atomic.Uint64
	var errRefused atomic.Uint64
	var errAddrNotAvail atomic.Uint64
	var errUnreach atomic.Uint64
	var errPerm atomic.Uint64
	var errOther atomic.Uint64

	dialer := &net.Dialer{KeepAlive: -1}
	if *tuneSocketFlag {
		dialer.Control = tcpFastControl
	}

	var hostLimiters sync.Map

	var printedErr atomic.Uint64
	var workersWG sync.WaitGroup
	workersWG.Add(*workersFlag)
	for i := 0; i < *workersFlag; i++ {
		go func() {
			defer workersWG.Done()
			for job := range jobs {
				hostLimiter := acquireHostToken(job.ip, perHostInflight, &hostLimiters)
				addr := net.JoinHostPort(job.ip, strconv.Itoa(job.port))
				conn, dialErr := dialWithRetries(ctx, dialer, addr, *timeoutFlag, *retriesFlag, *retryBackoffFlag)
				if hostLimiter != nil {
					<-hostLimiter
				}
				if dialErr == nil {
					_ = conn.Close()
					openCount.Add(1)
					openResults <- addr
				} else if *showClosedFlag {
					n := printedErr.Add(1)
					if *errPrintLimitFlag == 0 || int(n) <= *errPrintLimitFlag {
						errResults <- fmt.Sprintf("%s: %v", addr, dialErr)
					}
				}
				if dialErr != nil {
					switch classifyDialErr(dialErr) {
					case dialErrTimeout:
						errTimeout.Add(1)
					case dialErrRefused:
						errRefused.Add(1)
					case dialErrAddrNotAvail:
						errAddrNotAvail.Add(1)
					case dialErrUnreachable:
						errUnreach.Add(1)
					case dialErrPerm:
						errPerm.Add(1)
					case dialErrOther:
						errOther.Add(1)
					}
				}
				scanned.Add(1)
			}
		}()
	}

	var collectorWG sync.WaitGroup
	openList := make([]string, 0, 1024)
	var progressLine atomic.Value // string

	collectorWG.Add(1)
	go func() {
		defer collectorWG.Done()

		style := pickProgressStyle(*progressStyleFlag)
		var ticker *time.Ticker
		if *progressFlag {
			ticker = time.NewTicker(*progressIntervalFlag)
			defer ticker.Stop()
		}

		lastAt := time.Now()
		var prev uint64

		redraw := func() {
			if !*progressFlag {
				return
			}
			v := progressLine.Load()
			if v == nil {
				return
			}
			writeProgress(os.Stderr, style, v.(string))
		}

		printLine := func(dst *os.File, s string) {
			if *progressFlag && style == progressStyleCR {
				clearProgress(dst)
			}
			fmt.Fprintln(dst, s)
			redraw()
		}

		openCh := openResults
		errCh := errResults
		for openCh != nil || errCh != nil {
			select {
			case addr, ok := <-openCh:
				if !ok {
					openCh = nil
					continue
				}
				if *streamOpenFlag {
					printLine(os.Stdout, "open "+addr)
				} else {
					openList = append(openList, addr)
				}
			case line, ok := <-errCh:
				if !ok {
					errCh = nil
					continue
				}
				// Closed errors are already rate-limited.
				printLine(os.Stderr, line)
			case <-func() <-chan time.Time {
				if ticker == nil {
					return nil
				}
				return ticker.C
			}():
				cur := scanned.Load()
				open := openCount.Load()
				delta := cur - prev
				prev = cur
				now := time.Now()
				interval := now.Sub(lastAt).Seconds()
				lastAt = now

				elapsed := time.Since(start).Seconds()
				avgRate := float64(cur) / max(elapsed, 0.001)
				curRate := float64(delta) / max(interval, 0.001)
				eta := "-"
				if curRate > 1 && cur < totalTasks {
					remain := float64(totalTasks - cur)
					eta = (time.Duration(remain/curRate) * time.Second).Truncate(time.Second).String()
				}

				pct := float64(cur) / float64(maxU64(totalTasks, 1))
				bar := renderBar(pct, 24)
				line := fmt.Sprintf("%s %6.2f%% %d/%d open=%d cur=%.0f/s avg=%.0f/s eta=%s",
					bar, pct*100, cur, totalTasks, open, curRate, avgRate, eta)
				progressLine.Store(line)
				writeProgress(os.Stderr, style, line)
			}
		}

		// Final clean line break when using CR progress.
		if *progressFlag && style == progressStyleCR {
			clearProgress(os.Stderr)
		}
	}()

	// Port-major order spreads load across hosts and reduces per-host burst loss.
	for _, port := range ports {
		targets.ForEach(func(ip string) {
			jobs <- targetPort{ip: ip, port: port}
		})
	}
	close(jobs)

	workersWG.Wait()
	close(openResults)
	close(errResults)
	collectorWG.Wait()

	if !*streamOpenFlag {
		sort.Strings(openList)
		for _, addr := range openList {
			fmt.Println("open", addr)
		}
	}

	elapsed := time.Since(start)
	totalDone := scanned.Load()
	rate := float64(totalDone) / elapsed.Seconds()
	fmt.Printf(
		"done scanned=%d open=%d elapsed=%s avg=%.0f conn/s\n",
		totalDone,
		openCount.Load(),
		elapsed.Truncate(time.Millisecond),
		rate,
	)
	fmt.Printf(
		"errors timeout=%d refused=%d addr_not_avail=%d unreach=%d perm=%d other=%d\n",
		errTimeout.Load(),
		errRefused.Load(),
		errAddrNotAvail.Load(),
		errUnreach.Load(),
		errPerm.Load(),
		errOther.Load(),
	)
	if errPerm.Load() > 0 {
		fmt.Fprintln(os.Stderr, "note: saw 'operation not permitted' dial errors; this usually indicates firewall/sandbox/network policy blocking TCP connect.")
	}
}

func parsePorts(raw string) ([]int, error) {
	seen := make(map[int]struct{}, 2048)
	parts := strings.Split(raw, ",")
	ports := make([]int, 0, len(parts)*4)

	for _, p := range parts {
		part := strings.TrimSpace(p)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			if len(bounds) != 2 {
				return nil, errors.New("bad range format")
			}
			a, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
			if err != nil {
				return nil, err
			}
			b, err := strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil {
				return nil, err
			}
			if a > b {
				a, b = b, a
			}
			if a < 1 {
				a = 1
			}
			if b > 65535 {
				b = 65535
			}
			for v := a; v <= b; v++ {
				if _, ok := seen[v]; ok {
					continue
				}
				seen[v] = struct{}{}
				ports = append(ports, v)
			}
			continue
		}

		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		if v < 1 || v > 65535 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		ports = append(ports, v)
	}

	sort.Ints(ports)
	return ports, nil
}

func gatherTargetInputs(targetsRaw, targetsFile string) ([]string, error) {
	var out []string

	addRaw := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		// Support -targets @path as a convenience shortcut.
		if strings.HasPrefix(s, "@") && len(s) > 1 {
			targetsFile = strings.TrimSpace(s[1:])
			return
		}
		out = append(out, splitTargetsList(s)...)
	}

	addRaw(targetsRaw)

	if targetsFile != "" {
		lines, err := readTargetsFile(targetsFile)
		if err != nil {
			return nil, err
		}
		for _, ln := range lines {
			out = append(out, splitTargetsList(ln)...)
		}
	}

	return out, nil
}

func readTargetsFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip inline comments: "10.0.0.0/24  # office"
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func splitTargetsList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

type ipv4Range struct {
	start uint32
	end   uint32
}

type Targets struct {
	singles []string
	ranges4 []ipv4Range
}

func (t Targets) Count() uint64 {
	var n uint64
	n += uint64(len(t.singles))
	for _, r := range t.ranges4 {
		if r.end >= r.start {
			n += uint64(r.end-r.start) + 1
		}
	}
	return n
}

func (t Targets) ForEach(fn func(ip string)) {
	for _, s := range t.singles {
		fn(s)
	}
	for _, r := range t.ranges4 {
		for u := r.start; u <= r.end; u++ {
			fn(ipv4ToString(u))
			if u == ^uint32(0) {
				break
			}
		}
	}
}

func parseTargets(inputs []string) (Targets, error) {
	seen := make(map[string]struct{}, len(inputs))
	var out Targets

	addSingle := func(ip string) {
		if _, ok := seen[ip]; ok {
			return
		}
		seen[ip] = struct{}{}
		out.singles = append(out.singles, ip)
	}

	addRangeKey := func(key string, r ipv4Range) {
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out.ranges4 = append(out.ranges4, r)
	}

	for _, raw := range inputs {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}

		// IPv4 range: a-b (no CIDR slash)
		if strings.Contains(part, "-") && !strings.Contains(part, "/") {
			a, b, ok := splitRange(part)
			if ok {
				ra, okA := parseIPv4U32(a)
				rb, okB := parseIPv4U32(b)
				if okA && okB {
					if ra > rb {
						ra, rb = rb, ra
					}
					key := "range:" + ipv4ToString(ra) + "-" + ipv4ToString(rb)
					addRangeKey(key, ipv4Range{start: ra, end: rb})
					continue
				}
			}
		}

		if ip := net.ParseIP(part); ip != nil {
			addSingle(ip.String())
			continue
		}

		ip, ipNet, err := net.ParseCIDR(part)
		if err != nil {
			return Targets{}, fmt.Errorf("bad target %q", part)
		}

		start, end, ok := cidrToIPv4Range(ip, ipNet)
		if ok {
			key := "cidr:" + ipNet.String()
			addRangeKey(key, ipv4Range{start: start, end: end})
			continue
		}

		// IPv6 CIDRs are intentionally not expanded (too large). Treat as error to avoid surprises.
		return Targets{}, fmt.Errorf("unsupported (non-IPv4) CIDR %q", part)
	}

	return out, nil
}

func splitRange(s string) (string, string, bool) {
	ab := strings.SplitN(s, "-", 2)
	if len(ab) != 2 {
		return "", "", false
	}
	a := strings.TrimSpace(ab[0])
	b := strings.TrimSpace(ab[1])
	if a == "" || b == "" {
		return "", "", false
	}
	return a, b, true
}

func parseIPv4U32(s string) (uint32, bool) {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return 0, false
	}
	v4 := ip.To4()
	if v4 == nil {
		return 0, false
	}
	return binary.BigEndian.Uint32(v4), true
}

func cidrToIPv4Range(ip net.IP, ipNet *net.IPNet) (uint32, uint32, bool) {
	v4 := ip.To4()
	if v4 == nil {
		return 0, 0, false
	}
	mask := ipNet.Mask
	if len(mask) != net.IPv4len {
		return 0, 0, false
	}

	startIP := ip.Mask(mask).To4()
	if startIP == nil {
		return 0, 0, false
	}
	start := binary.BigEndian.Uint32(startIP)
	m := binary.BigEndian.Uint32(mask)
	end := start | ^m

	// Drop network and broadcast only when there is a host range (>= 4 addresses).
	if end-start+1 >= 4 {
		start++
		end--
	}
	return start, end, true
}

func ipv4ToString(u uint32) string {
	b0 := byte(u >> 24)
	b1 := byte(u >> 16)
	b2 := byte(u >> 8)
	b3 := byte(u)
	buf := make([]byte, 0, 15)
	buf = strconv.AppendInt(buf, int64(b0), 10)
	buf = append(buf, '.')
	buf = strconv.AppendInt(buf, int64(b1), 10)
	buf = append(buf, '.')
	buf = strconv.AppendInt(buf, int64(b2), 10)
	buf = append(buf, '.')
	buf = strconv.AppendInt(buf, int64(b3), 10)
	return string(buf)
}

func cloneIP(ip net.IP) net.IP {
	dst := make(net.IP, len(ip))
	copy(dst, ip)
	return dst
}

func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			return
		}
	}
}

func isIPv4Mask(mask net.IPMask) bool {
	return len(mask) == net.IPv4len
}

type progressStyle uint8

const (
	progressStyleCR progressStyle = iota
	progressStyleLine
)

func pickProgressStyle(s string) progressStyle {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cr":
		return progressStyleCR
	case "line":
		return progressStyleLine
	case "auto":
		// If stderr is a terminal, use carriage-return updates; otherwise print lines.
		if fi, err := os.Stderr.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			return progressStyleCR
		}
		return progressStyleLine
	default:
		return progressStyleCR
	}
}

func writeProgress(w *os.File, style progressStyle, line string) {
	switch style {
	case progressStyleLine:
		fmt.Fprintln(w, line)
	default:
		// Clear the current line, then rewrite progress without adding a newline.
		fmt.Fprintf(w, "\r\033[2K%s", line)
	}
}

func clearProgress(w *os.File) {
	fmt.Fprint(w, "\r\033[2K")
}

func renderBar(pct float64, width int) string {
	if width <= 0 {
		return "[]"
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func acquireHostToken(host string, max int, m *sync.Map) chan struct{} {
	if max == 0 {
		return nil
	}
	v, _ := m.LoadOrStore(host, make(chan struct{}, max))
	ch := v.(chan struct{})
	ch <- struct{}{}
	return ch
}

func dialWithRetries(
	parent context.Context,
	dialer *net.Dialer,
	addr string,
	baseTimeout time.Duration,
	retries int,
	backoff time.Duration,
) (net.Conn, error) {
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		timeout := baseTimeout << attempt
		if timeout <= 0 {
			timeout = baseTimeout
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if attempt == retries || !isRetryableDialErr(err) {
			return nil, err
		}
		if backoff > 0 {
			time.Sleep(backoff << attempt)
		}
	}
	return nil, lastErr
}

func isRetryableDialErr(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, syscall.EADDRNOTAVAIL) ||
		errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) ||
		errors.Is(err, syscall.EAGAIN) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "too many open files") ||
		strings.Contains(msg, "cannot assign requested address") ||
		strings.Contains(msg, "can't assign requested address") ||
		strings.Contains(msg, "no buffer space available") ||
		strings.Contains(msg, "resource temporarily unavailable")
}

type dialErrClass uint8

const (
	dialErrOther dialErrClass = iota
	dialErrTimeout
	dialErrRefused
	dialErrAddrNotAvail
	dialErrUnreachable
	dialErrPerm
)

func classifyDialErr(err error) dialErrClass {
	if err == nil {
		return dialErrOther
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return dialErrTimeout
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return dialErrRefused
	}
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		return dialErrAddrNotAvail
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return dialErrUnreachable
	}
	if errors.Is(err, syscall.EPERM) {
		return dialErrPerm
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "operation not permitted"):
		return dialErrPerm
	case strings.Contains(msg, "connection refused"):
		return dialErrRefused
	case strings.Contains(msg, "cannot assign requested address") || strings.Contains(msg, "can't assign requested address"):
		return dialErrAddrNotAvail
	case strings.Contains(msg, "no route to host") || strings.Contains(msg, "network is unreachable") || strings.Contains(msg, "host is down"):
		return dialErrUnreachable
	case strings.Contains(msg, "timeout"):
		return dialErrTimeout
	default:
		return dialErrOther
	}
}

func tcpFastControl(network, _ string, raw syscall.RawConn) error {
	if !strings.HasPrefix(network, "tcp") {
		return nil
	}

	// Optional tuning. Ignore failures to keep compatibility across systems.
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4096)
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
	})
	return nil
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
