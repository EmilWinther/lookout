// portping - TCP/HTTP service monitor with a live terminal dashboard.
// Zero dependencies beyond the Go standard library.
//
// Usage:
//
//	portping [options] [host:port ...]
//	portping -config monitors.toml
//
// Examples:
//
//	portping google.com:443 localhost:5432 redis.internal:6379
//	portping -interval 2s -timeout 1s api.myapp.com:443 db.myapp.com:5432
//	portping -config monitors.toml
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const version = "1.0.0"

// ── ANSI helpers ────────────────────────────────────────────────────────────

const (
	clrReset      = "\033[0m"
	clrBold       = "\033[1m"
	clrDim        = "\033[2m"
	clrRed        = "\033[31m"
	clrGreen      = "\033[32m"
	clrYellow     = "\033[33m"
	clrCyan       = "\033[36m"
	clrWhite      = "\033[97m"
	clrBgRed      = "\033[41m"
	clrBgGreen    = "\033[42m"
	clrBgDarkGray = "\033[100m"

	cursorHide = "\033[?25l"
	cursorShow = "\033[?25h"
	clearLine  = "\033[2K"
	cursorUp   = "\033[%dA"
	moveBOL    = "\r"
)

func esc(codes ...string) string {
	return strings.Join(codes, "")
}

// ── Config / Target ─────────────────────────────────────────────────────────

type Protocol string

const (
	ProtoTCP  Protocol = "tcp"
	ProtoHTTP Protocol = "http"
)

// Target is one service to monitor.
type Target struct {
	Label    string
	Host     string
	Port     int
	Proto    Protocol
	HTTPPath string // only for http targets
}

func (t *Target) Addr() string {
	return fmt.Sprintf("%s:%d", t.Host, t.Port)
}

func (t *Target) Display() string {
	if t.Label != "" {
		return t.Label
	}
	if t.Proto == ProtoHTTP {
		scheme := "http"
		if t.Port == 443 {
			scheme = "https"
		}
		return fmt.Sprintf("%s://%s:%d%s", scheme, t.Host, t.Port, t.HTTPPath)
	}
	return t.Addr()
}

// parseTarget parses a string like:
//
//	host:port
//	label=host:port
//	http://host:port/path
//	label=http://host:port/path
func parseTarget(s string) (*Target, error) {
	label := ""
	if idx := strings.Index(s, "="); idx != -1 {
		label = s[:idx]
		s = s[idx+1:]
	}

	t := &Target{Label: label, Proto: ProtoTCP, HTTPPath: "/"}

	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		t.Proto = ProtoHTTP
		raw := s
		scheme := "http"
		if strings.HasPrefix(raw, "https://") {
			scheme = "https"
			raw = raw[len("https://"):]
		} else {
			raw = raw[len("http://"):]
		}
		slash := strings.Index(raw, "/")
		if slash != -1 {
			t.HTTPPath = raw[slash:]
			raw = raw[:slash]
		}
		host, portStr, err := net.SplitHostPort(raw)
		if err != nil {
			// No explicit port — use default
			if scheme == "https" {
				t.Port = 443
			} else {
				t.Port = 80
			}
			t.Host = raw
		} else {
			t.Host = host
			p, err := strconv.Atoi(portStr)
			if err != nil {
				return nil, fmt.Errorf("invalid port in %q", s)
			}
			t.Port = p
		}
		return t, nil
	}

	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return nil, fmt.Errorf("invalid target %q: expected host:port", s)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port in %q", s)
	}
	t.Host = host
	t.Port = p
	return t, nil
}

// parseTOMLConfig reads a minimal hand-rolled TOML-ish config.
// Supports sections like:
//
//	[[target]]
//	label = "My DB"
//	host  = "localhost"
//	port  = 5432
//	proto = "tcp"
func parseTOMLConfig(path string) ([]*Target, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var targets []*Target
	var cur *Target

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "[[target]]" {
			if cur != nil {
				targets = append(targets, cur)
			}
			cur = &Target{Proto: ProtoTCP, HTTPPath: "/"}
			continue
		}
		if cur == nil {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), `"`)
		switch key {
		case "label":
			cur.Label = val
		case "host":
			cur.Host = val
		case "port":
			p, _ := strconv.Atoi(val)
			cur.Port = p
		case "proto":
			cur.Proto = Protocol(strings.ToLower(val))
		case "path":
			cur.HTTPPath = val
		}
	}
	if cur != nil {
		targets = append(targets, cur)
	}
	return targets, scanner.Err()
}

// ── Check result ─────────────────────────────────────────────────────────────

type Status int

const (
	StatusUp      Status = iota
	StatusDown    Status = iota
	StatusPending Status = iota
)

type CheckResult struct {
	Target    *Target
	Status    Status
	Latency   time.Duration
	Err       string
	CheckedAt time.Time
	// rolling history: true=up, false=down (last N checks)
	History []bool
	// running stats
	TotalChecks int
	TotalUp     int
	MinLatency  time.Duration
	MaxLatency  time.Duration
	SumLatency  time.Duration // for mean
}

func (r *CheckResult) Uptime() float64 {
	if r.TotalChecks == 0 {
		return 0
	}
	return float64(r.TotalUp) / float64(r.TotalChecks) * 100
}

func (r *CheckResult) AvgLatency() time.Duration {
	if r.TotalUp == 0 {
		return 0
	}
	return r.SumLatency / time.Duration(r.TotalUp)
}

const historyLen = 20

func (r *CheckResult) pushHistory(up bool) {
	r.History = append(r.History, up)
	if len(r.History) > historyLen {
		r.History = r.History[len(r.History)-historyLen:]
	}
}

// ── Checker ──────────────────────────────────────────────────────────────────

type Checker struct {
	target  *Target
	timeout time.Duration
	result  *CheckResult
	mu      sync.RWMutex
}

func newChecker(t *Target, timeout time.Duration) *Checker {
	return &Checker{
		target:  t,
		timeout: timeout,
		result: &CheckResult{
			Target: t,
			Status: StatusPending,
		},
	}
}

func (c *Checker) check() {
	start := time.Now()
	var (
		up  bool
		msg string
	)

	switch c.target.Proto {
	case ProtoHTTP:
		up, msg = c.checkHTTP()
	default:
		up, msg = c.checkTCP()
	}

	latency := time.Since(start)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.result.CheckedAt = time.Now()
	c.result.TotalChecks++

	if up {
		c.result.Status = StatusUp
		c.result.Latency = latency
		c.result.Err = ""
		c.result.TotalUp++
		c.result.SumLatency += latency
		if c.result.MinLatency == 0 || latency < c.result.MinLatency {
			c.result.MinLatency = latency
		}
		if latency > c.result.MaxLatency {
			c.result.MaxLatency = latency
		}
	} else {
		c.result.Status = StatusDown
		c.result.Latency = 0
		c.result.Err = msg
	}
	c.result.pushHistory(up)
}

func (c *Checker) checkTCP() (bool, string) {
	conn, err := net.DialTimeout("tcp", c.target.Addr(), c.timeout)
	if err != nil {
		return false, shortErr(err)
	}
	conn.Close()
	return true, ""
}

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return nil // follow redirects
	},
}

func (c *Checker) checkHTTP() (bool, string) {
	scheme := "http"
	if c.target.Port == 443 || strings.Contains(c.target.Display(), "https://") {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s:%d%s", scheme, c.target.Host, c.target.Port, c.target.HTTPPath)
	httpClient.Timeout = c.timeout
	resp, err := httpClient.Get(url)
	if err != nil {
		return false, shortErr(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return true, ""
}

func shortErr(err error) string {
	s := err.Error()
	// Trim long "dial tcp ..." prefix for cleaner display
	if idx := strings.Index(s, ": "); idx != -1 {
		trimmed := s[idx+2:]
		// Try one more level
		if idx2 := strings.Index(trimmed, ": "); idx2 != -1 {
			trimmed = trimmed[idx2+2:]
		}
		return trimmed
	}
	return s
}

func (c *Checker) Result() *CheckResult {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// shallow copy is fine for display
	r := *c.result
	return &r
}

// ── Dashboard renderer ───────────────────────────────────────────────────────

const (
	barWidth     = 20 // history bar width
	labelWidth   = 28 // display name column
	latencyWidth = 9
	uptimeWidth  = 8
	statusWidth  = 10
)

func statusBadge(s Status) string {
	switch s {
	case StatusUp:
		return esc(clrBgGreen, clrBold, " UP ") + clrReset
	case StatusDown:
		return esc(clrBgRed, clrBold, " DOWN ") + clrReset
	default:
		return esc(clrBgDarkGray, " … ") + clrReset
	}
}

func historyBar(h []bool) string {
	const full = historyLen
	pad := full - len(h)
	var b strings.Builder
	for i := 0; i < pad; i++ {
		b.WriteString(esc(clrDim, "·"))
	}
	for _, up := range h {
		if up {
			b.WriteString(esc(clrGreen, "█"))
		} else {
			b.WriteString(esc(clrRed, "░"))
		}
	}
	b.WriteString(clrReset)
	return b.String()
}

func fmtLatency(d time.Duration) string {
	if d == 0 {
		return esc(clrDim, "  —  ")
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%4dµs", d.Microseconds())
	}
	return fmt.Sprintf("%5dms", d.Milliseconds())
}

func fmtUptime(pct float64, checks int) string {
	if checks == 0 {
		return esc(clrDim, "  —  ")
	}
	color := clrGreen
	if pct < 99 {
		color = clrYellow
	}
	if pct < 90 {
		color = clrRed
	}
	return esc(color, fmt.Sprintf("%5.1f%%", pct))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Dashboard holds the terminal renderer.
type Dashboard struct {
	checkers []*Checker
	mu       sync.Mutex
	lines    int // how many lines we drew last frame
}

func newDashboard(checkers []*Checker) *Dashboard {
	return &Dashboard{checkers: checkers}
}

var startTime = time.Now()

func (d *Dashboard) header() string {
	elapsed := time.Since(startTime).Round(time.Second)
	return fmt.Sprintf("%s%s portping v%s%s  %s running %s  %s\n",
		clrBold, clrCyan, version, clrReset,
		esc(clrDim), elapsed.String(), clrReset,
	)
}

func (d *Dashboard) colHeader() string {
	return fmt.Sprintf(
		"%s%-*s  %-*s  %s  %-*s  %-*s  %s%s\n",
		esc(clrDim, clrBold),
		labelWidth, "TARGET",
		statusWidth, "STATUS",
		strings.Repeat(" ", historyLen-len("LAST 20 CHECKS")/2)+"LAST 20 CHECKS",
		latencyWidth, "LATENCY",
		uptimeWidth, "UPTIME",
		"AVG / MIN / MAX",
		clrReset,
	)
}

func (d *Dashboard) divider() string {
	return esc(clrDim) +
		strings.Repeat("─", labelWidth+statusWidth+historyLen+latencyWidth+uptimeWidth+30) +
		clrReset + "\n"
}

func (d *Dashboard) renderRow(r *CheckResult) string {
	display := truncate(r.Target.Display(), labelWidth)

	badge := statusBadge(r.Status)

	hist := historyBar(r.History)

	lat := fmtLatency(r.Latency)

	uptime := fmtUptime(r.Uptime(), r.TotalChecks)

	var extra string
	if r.Status == StatusDown && r.Err != "" {
		extra = esc(clrRed, clrDim, "  ✗ "+truncate(r.Err, 35))
	} else if r.TotalUp > 0 {
		extra = fmt.Sprintf("  %s%s%s / %s%s%s / %s%s%s",
			clrDim, fmtLatency(r.AvgLatency()), clrReset,
			clrDim, fmtLatency(r.MinLatency), clrReset,
			clrDim, fmtLatency(r.MaxLatency), clrReset,
		)
	}

	// Pad display name (no ANSI codes so len() is accurate)
	paddedDisplay := display + strings.Repeat(" ", labelWidth-len(display))

	return fmt.Sprintf("%s%s  %s  %s  %s%s  %s%s%s\n",
		clrWhite, paddedDisplay,
		badge,
		hist,
		clrCyan, lat, clrReset,
		uptime,
		extra+clrReset,
	)
}

func (d *Dashboard) Render() {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Collect results and sort: down first, then by display name
	results := make([]*CheckResult, len(d.checkers))
	for i, c := range d.checkers {
		results[i] = c.Result()
	}
	sort.Slice(results, func(i, j int) bool {
		si, sj := results[i].Status, results[j].Status
		if si != sj {
			return si < sj // Down(1) < Up(0) — show down at top
		}
		return results[i].Target.Display() < results[j].Target.Display()
	})

	var buf strings.Builder

	// Move cursor back up over previous frame
	if d.lines > 0 {
		fmt.Fprintf(&buf, "\033[%dA", d.lines)
	}

	linesDrawn := 0
	writeln := func(s string) {
		fmt.Fprint(&buf, moveBOL+clearLine+s)
		linesDrawn++
	}

	writeln(d.header())
	writeln(d.colHeader())
	writeln(d.divider())
	for _, r := range results {
		writeln(d.renderRow(r))
	}
	writeln(d.divider())
	writeln(fmt.Sprintf("%sPress Ctrl+C to quit%s\n", clrDim, clrReset))

	d.lines = linesDrawn
	fmt.Print(buf.String())
}

// ── Main ─────────────────────────────────────────────────────────────────────

func usage() {
	fmt.Fprintf(os.Stderr, `
%sportping%s v%s — TCP/HTTP service monitor
%s
Usage:
  portping [options] [target ...]
  portping -config <file>

Targets:
  host:port                    TCP probe  (e.g. localhost:5432)
  label=host:port              TCP probe with custom label
  http://host:port/path        HTTP probe (200–499 = up)
  https://host/path            HTTPS probe on port 443
  label=https://api.acme.com   HTTP probe with custom label

Options:
`, clrBold+clrCyan, clrReset, version, clrReset)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Config file (TOML):
  [[target]]
  label = "Postgres"
  host  = "localhost"
  port  = 5432
  proto = "tcp"

  [[target]]
  label = "API"
  host  = "api.example.com"
  port  = 443
  proto = "http"
  path  = "/healthz"

Examples:
  portping localhost:5432 redis.internal:6379
  portping -interval 2s -timeout 1s api.acme.com:443
  portping "DB=localhost:5432" "Cache=localhost:6379"
  portping -config monitors.toml
`)
}

func main() {
	interval := flag.Duration("interval", 5*time.Second, "Probe interval")
	timeout := flag.Duration("timeout", 3*time.Second, "Probe timeout")
	configFile := flag.String("config", "", "TOML config file")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("portping v" + version)
		os.Exit(0)
	}

	var targets []*Target

	if *configFile != "" {
		ts, err := parseTOMLConfig(*configFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading config: %v\n", err)
			os.Exit(1)
		}
		targets = ts
	}

	for _, arg := range flag.Args() {
		t, err := parseTarget(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		targets = append(targets, t)
	}

	if len(targets) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	// Build checkers
	checkers := make([]*Checker, len(targets))
	for i, t := range targets {
		checkers[i] = newChecker(t, *timeout)
	}

	// Hide cursor, restore on exit
	fmt.Print(cursorHide)
	defer fmt.Print(cursorShow)

	dash := newDashboard(checkers)

	// Initial probes (parallel)
	var wg sync.WaitGroup
	for _, c := range checkers {
		wg.Add(1)
		go func(c *Checker) {
			defer wg.Done()
			c.check()
		}(c)
	}
	wg.Wait()
	dash.Render()

	// Staggered probe loop
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for range ticker.C {
		var wg sync.WaitGroup
		for _, c := range checkers {
			wg.Add(1)
			go func(c *Checker) {
				defer wg.Done()
				c.check()
			}(c)
		}
		wg.Wait()
		dash.Render()
	}
}
