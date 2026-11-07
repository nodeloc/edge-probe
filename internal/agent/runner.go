package agent

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"runtime"
	"sort"
	"sync"
	"time"
)

const (
	defaultSyncSeconds = 300
	defaultTCPSeconds  = 60
	defaultHTTPSeconds = 3600
	defaultHTTPBytes   = 32 << 10
	maxHTTPBytes       = 64 << 10
	tcpConcurrency     = 8
	updateRetry        = time.Hour
	unauthorizedRetry  = 30 * time.Minute
	firstProbeDelay    = 5 * time.Second
	maxResolveNames    = 8
	resolveTimeout     = 5 * time.Second
)

// Runner 定期拨测服务端下发的目标，并按周期上报汇总结果
type Runner struct {
	Config   *Config
	Client   *Client
	Updater  *Updater
	Budget   *Budget
	Recorder *Recorder
	Logf     func(format string, args ...any)
	// Restart 程序替换后调用，由 main 负责重新执行新程序
	Restart func() error
	// LookupIP 用本机 DNS 解析域名，测试中可以替换
	LookupIP func(ctx context.Context, name string) ([]string, error)

	mu          sync.Mutex
	targets     []Target
	nonce       string
	syncSeconds int
	tcpSeconds  int
	httpSeconds int
	httpBytes   int
	resolve     []string
	clientIP    string
	lastHTTP    map[int64]time.Time
	lastUpdate  time.Time
}

func NewRunner(config *Config, configPath string) *Runner {
	return &Runner{
		Config:   config,
		Client:   NewClient(config.Server),
		Updater:  NewUpdater(config.Server),
		Budget:   NewBudget(configPath+".budget", config.MonthlyLimitMB),
		Recorder: NewRecorder(),
		Logf:     func(string, ...any) {},
		LookupIP: lookupIP,
		lastHTTP: map[int64]time.Time{},
	}
}

func (r *Runner) Run(ctx context.Context) error {
	go r.probeLoop(ctx)
	var backoff = 30 * time.Second
	for {
		var wait time.Duration
		err := r.SyncOnce(ctx)
		switch {
		case err == nil:
			backoff = 30 * time.Second
			wait = jitter(time.Duration(r.settings().syncSeconds) * time.Second)
		case IsUnauthorized(err):
			r.Logf("server rejected this agent (%v); it may have been removed or disabled in the console, retrying in %s", err, unauthorizedRetry)
			r.setTargets(nil)
			wait = unauthorizedRetry
		default:
			r.Logf("sync failed: %v; retrying in %s", err, backoff)
			wait = backoff
			if backoff *= 2; backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// SyncOnce 上报当前周期的结果并取回新的目标和 nonce
func (r *Runner) SyncOnce(ctx context.Context) error {
	r.mu.Lock()
	var targets, nonce, names = r.targets, r.nonce, r.resolve
	r.mu.Unlock()
	var results = r.Recorder.Take(targets)
	resp, err := r.Client.Sync(ctx, r.Config, &SyncRequest{Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Nonce: nonce, Results: results, Resolves: r.resolveAll(ctx, names)})
	r.Budget.Save()
	if err != nil {
		return err
	}
	r.apply(resp)
	r.Logf("synced %d results; %d targets; %.1f MB used this month", len(results), len(resp.Targets), float64(r.Budget.Used())/(1<<20))
	if resp.LatestVersion != "" && resp.LatestVersion != Version && time.Since(r.lastUpdate) >= updateRetry && r.Updater != nil {
		r.lastUpdate = time.Now()
		updated, err := r.Updater.Update(ctx)
		if err != nil {
			r.Logf("update to %s failed: %v", resp.LatestVersion, err)
		} else if updated && r.Restart != nil {
			r.Logf("updated to %s, restarting", resp.LatestVersion)
			return r.Restart()
		}
	}
	return nil
}

func (r *Runner) apply(resp *SyncResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nonce = resp.Nonce
	r.clientIP = resp.ClientIP
	r.syncSeconds = clamp(resp.SyncSeconds, 60, 3600, defaultSyncSeconds)
	r.tcpSeconds = clamp(resp.TCPSeconds, 10, 3600, defaultTCPSeconds)
	r.httpSeconds = clamp(resp.HTTPSeconds, 300, 86400, defaultHTTPSeconds)
	r.httpBytes = clamp(resp.HTTPBytes, 1024, maxHTTPBytes, defaultHTTPBytes)
	r.targets = resp.Targets
	if len(resp.Resolve) > maxResolveNames {
		resp.Resolve = resp.Resolve[:maxResolveNames]
	}
	r.resolve = resp.Resolve
	// 新目标的 HTTP 拨测随机错开，避免所有 agent 同时下载
	var known = map[int64]bool{}
	for _, t := range resp.Targets {
		known[t.ID] = true
		if _, ok := r.lastHTTP[t.ID]; !ok {
			r.lastHTTP[t.ID] = time.Now().Add(-time.Duration(rand.Int63n(int64(r.httpSeconds) * int64(time.Second))))
		}
	}
	for id := range r.lastHTTP {
		if !known[id] {
			delete(r.lastHTTP, id)
		}
	}
}

func (r *Runner) setTargets(targets []Target) {
	r.mu.Lock()
	r.targets = targets
	r.mu.Unlock()
}

type runnerSettings struct {
	targets                              []Target
	nonce                                string
	syncSeconds, tcpSeconds, httpSeconds int
	httpBytes                            int
}

func (r *Runner) settings() runnerSettings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return runnerSettings{targets: r.targets, nonce: r.nonce, syncSeconds: clamp(r.syncSeconds, 60, 3600, defaultSyncSeconds), tcpSeconds: clamp(r.tcpSeconds, 10, 3600, defaultTCPSeconds), httpSeconds: r.httpSeconds, httpBytes: r.httpBytes}
}

func (r *Runner) probeLoop(ctx context.Context) {
	var started = false
	for {
		// 取得目标后尽快开始第一次拨测，之后按服务端下发的间隔
		var wait = firstProbeDelay
		if started {
			wait = jitter(time.Duration(r.settings().tcpSeconds) * time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if len(r.settings().targets) == 0 {
			continue
		}
		started = true
		r.ProbeOnce(ctx)
	}
}

// ProbeOnce 对所有目标做一次 TCP 拨测，并对到期的目标做 HTTP 拨测
func (r *Runner) ProbeOnce(ctx context.Context) {
	var s = r.settings()
	if len(s.targets) == 0 {
		return
	}
	// 有 nonce 时每轮在一条连接上测请求往返（节点签名）；还没有 nonce 时只测建连
	var rounds = s.nonce != ""
	var cost = int64(tcpProbeBytes)
	if rounds {
		cost = roundOverheadBytes + defaultRoundSamples*roundRequestBytes
	}
	if !r.Budget.Allow(int64(len(s.targets)) * cost) {
		return
	}
	var wg sync.WaitGroup
	var sem = make(chan struct{}, tcpConcurrency)
	for _, t := range s.targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(t Target) {
			defer func() { <-sem; wg.Done() }()
			if rounds && t.HTTPPort > 0 {
				r.Recorder.AddRound(t.ID, MeasureRound(ctx, t.IP, t.HTTPPort, s.nonce, defaultRoundSamples), defaultRoundSamples)
				return
			}
			rtt, err := MeasureTCP(ctx, t.IP, t.Port)
			r.Recorder.AddTCP(t.ID, rtt, err)
		}(t)
	}
	wg.Wait()

	if s.nonce == "" || s.httpBytes <= 0 {
		return
	}
	for _, t := range s.targets {
		if t.HTTPPort <= 0 || !r.httpDue(t.ID, s.httpSeconds) {
			continue
		}
		if !r.Budget.Allow(int64(s.httpBytes) + httpOverheadBytes) {
			return
		}
		r.Recorder.SetHTTP(t.ID, MeasureHTTP(ctx, t.IP, t.HTTPPort, s.nonce, s.httpBytes))
	}
}

// resolveAll 用本机的 DNS 解析服务端指定的域名，让平台看到用户网络实际解析到的节点
func (r *Runner) resolveAll(ctx context.Context, names []string) []Resolve {
	if len(names) == 0 || r.LookupIP == nil {
		return nil
	}
	var resolves = make([]Resolve, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
			defer cancel()
			var start = time.Now()
			ips, err := r.LookupIP(ctx, name)
			resolves[i] = Resolve{Name: name, IPs: ips, DurationUs: time.Since(start).Microseconds()}
			if err != nil {
				resolves[i].Error = err.Error()
			}
		}(i, name)
	}
	wg.Wait()
	return resolves
}

func lookupIP(ctx context.Context, name string) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, name)
	if err != nil {
		return nil, err
	}
	var ips = []string{}
	for _, addr := range addrs {
		ips = append(ips, addr.IP.String())
	}
	sort.Strings(ips)
	return ips, nil
}

// MarkHTTPDue 让所有目标的 HTTP 拨测立即到期，供 doctor 使用
func (r *Runner) MarkHTTPDue() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.lastHTTP {
		r.lastHTTP[id] = time.Time{}
	}
}

func (r *Runner) httpDue(id int64, seconds int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var last, ok = r.lastHTTP[id]
	if ok && time.Since(last) < time.Duration(seconds)*time.Second {
		return false
	}
	r.lastHTTP[id] = time.Now()
	return true
}

func clamp(v, min, max, def int) int {
	if v <= 0 {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// jitter ±10%，分散大量 agent 的请求
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - d/10 + time.Duration(rand.Int63n(int64(d/5)+1))
}

// Summary 取出当前结果并格式化，供 doctor 显示
func (r *Runner) Summary() []string {
	var lines = []string{}
	var clients = []string{}
	var results = r.Recorder.Take(r.settings().targets)
	for _, result := range results {
		if result.RTTClient != "" {
			clients = append(clients, result.RTTClient)
		}
		if result.HTTP != nil && result.HTTP.Client != "" {
			clients = append(clients, result.HTTP.Client)
		}
		var line = fmt.Sprintf("%s: tcp %d/%d", result.IP, result.TCPOK, result.TCPSent)
		if result.TCPOK > 0 {
			line += " " + formatMs(result.TCPMedianUs)
		}
		if result.RTTSent > 0 {
			line += fmt.Sprintf(", rtt %d/%d", result.RTTOK, result.RTTSent)
			if result.RTTOK > 0 {
				line += " " + formatMs(result.RTTMedianUs)
			}
		}
		if result.HTTP != nil {
			if result.HTTP.OK {
				line += ", http " + formatMs(result.HTTP.TotalUs)
			} else {
				line += ", http failed"
			}
		}
		lines = append(lines, line)
	}
	r.mu.Lock()
	var clientIP = r.clientIP
	r.mu.Unlock()
	if handshakeIntercepted(results) {
		return append(lines, "network: proxied local-handshake")
	}
	return append(lines, "network: "+networkPath(clientIP, clients, handshakeClean(results)))
}

// handshakeClean 多数节点的建连和请求往返接近，规则与平台的 probeHandshakeClean 一致
func handshakeClean(results []Result) bool {
	var total, hits = handshakeHits(results)
	return total >= 2 && hits*5 < total
}

// handshakeIntercepted 判断握手是否被本地代答，规则与平台的 probeResultsIntercepted 一致：
// 代理能代答握手，但请求必须到达节点。全部流量走同一个代理时，只有这个特征能识别
func handshakeIntercepted(results []Result) bool {
	var total, hits = handshakeHits(results)
	return total >= 2 && hits*5 >= total*4
}

func handshakeHits(results []Result) (total, hits int) {
	for _, result := range results {
		if result.TCPOK == 0 || result.RTTOK == 0 {
			continue
		}
		total++
		if result.TCPMedianUs > 0 && result.RTTMedianUs >= result.TCPMedianUs*4 && result.RTTMedianUs-result.TCPMedianUs >= 30000 {
			hits++
		}
	}
	return total, hits
}

// networkPath 判断到节点的路径，安装脚本按这里的第一个词给出提示。只比较网段：多出口的宽带可能被误报，
// 平台还会比较地区和运营商。握手确实到达了节点时，以节点看到的 IP 为准，平台的请求走代理不影响测量：
// 平台和控制台共用 Cloudflare 的 IP，用户不能为了拨测把它们设为直连
func networkPath(platformIP string, nodeIPs []string, clean bool) string {
	if len(nodeIPs) == 0 {
		return "unknown"
	}
	var mask = net.CIDRMask(24, 32)
	var first = net.ParseIP(nodeIPs[0]).To4()
	for _, nodeIP := range nodeIPs {
		var node = net.ParseIP(nodeIP).To4()
		if node == nil || first == nil || !node.Mask(mask).Equal(first.Mask(mask)) {
			return fmt.Sprintf("proxied platform=%s node=%s", platformIP, nodeIP)
		}
	}
	var platform = net.ParseIP(platformIP).To4()
	if platform != nil && platform.Mask(mask).Equal(first.Mask(mask)) {
		return "direct " + platformIP
	}
	if clean {
		return "direct " + nodeIPs[0]
	}
	if platform == nil {
		return "unknown"
	}
	return fmt.Sprintf("proxied platform=%s node=%s", platformIP, nodeIPs[0])
}

// ResolveSummary 用本机 DNS 解析服务端指定的域名，供 doctor 显示
func (r *Runner) ResolveSummary(ctx context.Context) []string {
	r.mu.Lock()
	var names = r.resolve
	r.mu.Unlock()
	var lines = []string{}
	for _, resolve := range r.resolveAll(ctx, names) {
		if resolve.Error != "" {
			lines = append(lines, fmt.Sprintf("resolve %s: %s", resolve.Name, resolve.Error))
		} else {
			lines = append(lines, fmt.Sprintf("resolve %s: %v (%s)", resolve.Name, resolve.IPs, formatMs(resolve.DurationUs)))
		}
	}
	return lines
}
