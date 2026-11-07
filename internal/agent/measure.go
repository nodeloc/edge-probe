package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	tcpTimeout  = 3 * time.Second
	httpTimeout = 15 * time.Second
	// 估算的单次 TCP 拨测流量：握手、关闭和包头
	tcpProbeBytes = 400
	// HTTP 请求头、响应头和握手的估算开销
	httpOverheadBytes = 1200
	// 一轮往返测量：握手加上每个请求和响应的头部
	roundOverheadBytes  = 400
	roundRequestBytes   = 600
	roundTimeout        = 8 * time.Second
	defaultRoundSamples = 2
)

// MeasureTCP 建立一次 TCP 连接所需的时间，连接成功后立即关闭
func MeasureTCP(ctx context.Context, ip string, port int) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, tcpTimeout)
	defer cancel()
	var start = time.Now()
	conn, err := probeDialer(0).DialContext(ctx, "tcp4", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return 0, err
	}
	var rtt = time.Since(start)
	_ = conn.Close()
	return rtt, nil
}

// Round 一轮拨测：建立连接的时间，以及在这条连接上若干个小请求的往返时间
type Round struct {
	Connect    time.Duration
	ConnectErr error
	RTTs       []time.Duration
	Failed     int
	Signature  string // 最后一个成功响应的 v2 签名
	Client     string // 节点看到的客户端 IP
	Bytes      int64  // 估算流量
}

// MeasureRound 建立一条连接，依次发送 samples 个小请求。往返时间从发出请求到读完响应头，
// 不包含握手；即使路由器上的透明代理代答了握手，往返时间仍然要经过真实节点
func MeasureRound(ctx context.Context, ip string, port int, nonce string, samples int) *Round {
	var round = &Round{Bytes: roundOverheadBytes}
	ctx, cancel := context.WithTimeout(ctx, roundTimeout)
	defer cancel()
	var start = time.Now()
	conn, err := probeDialer(0).DialContext(ctx, "tcp4", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		round.ConnectErr = err
		round.Failed = samples
		return round
	}
	round.Connect = time.Since(start)
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var reader = bufio.NewReader(conn)
	var request = "GET " + ProbePath + "?nonce=" + nonce + "&bytes=" + strconv.Itoa(RTTProbeBytes) + " HTTP/1.1\r\nHost: " + ip + "\r\nUser-Agent: edge-probe/" + Version + "\r\n\r\n"
	for i := 0; i < samples; i++ {
		round.Bytes += roundRequestBytes
		var sent = time.Now()
		if _, err = io.WriteString(conn, request); err != nil {
			round.Failed += samples - i
			return round
		}
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			round.Failed += samples - i
			return round
		}
		var rtt = time.Since(sent)
		n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, RTTProbeBytes+1))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || n != RTTProbeBytes || resp.Header.Get("X-Nodepend-Probe-Signature-V2") == "" {
			round.Failed++
			if resp.Close {
				round.Failed += samples - i - 1
				return round
			}
			continue
		}
		round.RTTs = append(round.RTTs, rtt)
		round.Signature = resp.Header.Get("X-Nodepend-Probe-Signature-V2")
		round.Client = resp.Header.Get("X-Nodepend-Probe-Client")
		if resp.Close {
			round.Failed += samples - i - 1
			return round
		}
	}
	return round
}

// MeasureHTTP 从节点下载一段固定大小的数据；节点用自身密钥对 nonce 签名，证明请求确实到达了该节点
func MeasureHTTP(ctx context.Context, ip string, port int, nonce string, size int) *HTTPResult {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	var result = &HTTPResult{}
	var target = "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + ProbePath + "?nonce=" + nonce + "&bytes=" + strconv.Itoa(size)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return result
	}
	req.Header.Set("User-Agent", "edge-probe/"+Version)
	var start = time.Now()
	var firstByte time.Time
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}))
	// 每次新建连接，结果包含建连时间，与真实用户的首次访问一致
	var client = &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil, DisableCompression: true, DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return probeDialer(0).DialContext(ctx, "tcp4", addr)
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, int64(size)+1))
	result.Bytes = n
	if err != nil || resp.StatusCode != http.StatusOK || n != int64(size) {
		return result
	}
	result.OK = true
	result.TTFBUs = firstByte.Sub(start).Microseconds()
	result.TotalUs = time.Since(start).Microseconds()
	result.Signature = resp.Header.Get("X-Nodepend-Probe-Signature")
	result.SignatureV2 = resp.Header.Get("X-Nodepend-Probe-Signature-V2")
	result.Client = resp.Header.Get("X-Nodepend-Probe-Client")
	return result
}

// window 一个目标在当前上报周期内的测量值
type window struct {
	sent int
	rtts []time.Duration
	http *HTTPResult

	rttSent   int
	rttValues []time.Duration
	signature string
	client    string
}

// Recorder 按目标累计测量值，上报时取出并清空
type Recorder struct {
	mu      sync.Mutex
	windows map[int64]*window
}

func NewRecorder() *Recorder {
	return &Recorder{windows: map[int64]*window{}}
}

func (r *Recorder) get(id int64) *window {
	var w = r.windows[id]
	if w == nil {
		w = &window{}
		r.windows[id] = w
	}
	return w
}

func (r *Recorder) AddTCP(id int64, rtt time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var w = r.get(id)
	w.sent++
	if err == nil {
		w.rtts = append(w.rtts, rtt)
	}
}

// AddRound 记录一轮拨测：建连结果计入 TCP，请求往返计入 RTT
func (r *Recorder) AddRound(id int64, round *Round, samples int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var w = r.get(id)
	w.sent++
	if round.ConnectErr == nil {
		w.rtts = append(w.rtts, round.Connect)
	}
	w.rttSent += samples
	w.rttValues = append(w.rttValues, round.RTTs...)
	if round.Signature != "" {
		w.signature, w.client = round.Signature, round.Client
	}
}

func (r *Recorder) SetHTTP(id int64, result *HTTPResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.get(id).http = result
}

// Take 汇总并清空；只返回仍在目标列表中的目标
func (r *Recorder) Take(targets []Target) []Result {
	r.mu.Lock()
	var windows = r.windows
	r.windows = map[int64]*window{}
	r.mu.Unlock()

	var results = []Result{}
	for _, t := range targets {
		var w = windows[t.ID]
		if w == nil || (w.sent == 0 && w.http == nil && w.rttSent == 0) {
			continue
		}
		var result = Result{TargetID: t.ID, IP: t.IP, TCPSent: w.sent, TCPOK: len(w.rtts), HTTP: w.http}
		result.TCPMinUs, result.TCPMedianUs, result.TCPP90Us = percentiles(w.rtts)
		if w.rttSent > 0 {
			result.RTTSent, result.RTTOK = w.rttSent, len(w.rttValues)
			result.RTTMinUs, result.RTTMedianUs, result.RTTP90Us = percentiles(w.rttValues)
			result.RTTSignature, result.RTTClient = w.signature, w.client
		}
		results = append(results, result)
	}
	return results
}

// percentiles 最小值、中位数和 P90（微秒）
func percentiles(values []time.Duration) (min, median, p90 int64) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[0].Microseconds(), values[(len(values)-1)/2].Microseconds(), values[(len(values)*9+9)/10-1].Microseconds()
}

func formatMs(us int64) string {
	return fmt.Sprintf("%.1fms", float64(us)/1000)
}
