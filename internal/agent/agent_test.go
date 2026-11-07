package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecorderSummarizesOnlyCurrentTargets(t *testing.T) {
	var r = NewRecorder()
	for _, ms := range []int{30, 10, 20, 50, 40, 60, 70, 80, 90, 100} {
		r.AddTCP(1, time.Duration(ms)*time.Millisecond, nil)
	}
	r.AddTCP(1, 0, os.ErrDeadlineExceeded)
	r.AddTCP(2, 5*time.Millisecond, nil)
	r.SetHTTP(1, &HTTPResult{OK: true, TotalUs: 1234})
	var results = r.Take([]Target{{ID: 1, IP: "192.0.2.1"}})
	if len(results) != 1 {
		t.Fatalf("removed target reported: %+v", results)
	}
	var got = results[0]
	if got.TCPSent != 11 || got.TCPOK != 10 || got.TCPMinUs != 10000 || got.TCPMedianUs != 50000 || got.TCPP90Us != 90000 || got.HTTP == nil || got.HTTP.TotalUs != 1234 {
		t.Fatalf("unexpected summary %+v", got)
	}
	if len(r.Take([]Target{{ID: 1}})) != 0 {
		t.Fatal("results were not cleared after being taken")
	}
}

func TestMeasureTCP(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	var port = listener.Addr().(*net.TCPAddr).Port
	if rtt, err := MeasureTCP(context.Background(), "127.0.0.1", port); err != nil || rtt <= 0 {
		t.Fatalf("rtt=%s err=%v", rtt, err)
	}
	_ = listener.Close()
	if _, err := MeasureTCP(context.Background(), "127.0.0.1", port); err == nil {
		t.Fatal("closed port measured as reachable")
	}
}

func probeHandler(size int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ProbePath || r.URL.Query().Get("nonce") != "n1" {
			http.NotFound(w, r)
			return
		}
		var bytes = r.URL.Query().Get("bytes")
		w.Header().Set("X-Nodepend-Probe-Signature", "sig-"+bytes)
		w.Header().Set("X-Nodepend-Probe-Signature-V2", "sig2-"+bytes)
		w.Header().Set("X-Nodepend-Probe-Client", "203.0.113.7")
		if bytes == "1" {
			_, _ = w.Write([]byte{0})
			return
		}
		_, _ = w.Write(make([]byte, size))
	})
}

func serverPort(server *httptest.Server) int {
	port, _ := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
	return port
}

func probeServer(t *testing.T, size int) (*httptest.Server, int) {
	var server = httptest.NewServer(probeHandler(size))
	t.Cleanup(server.Close)
	return server, serverPort(server)
}

func TestMeasureHTTP(t *testing.T) {
	_, port := probeServer(t, 4096)
	var result = MeasureHTTP(context.Background(), "127.0.0.1", port, "n1", 4096)
	if !result.OK || result.Bytes != 4096 || result.Signature != "sig-4096" || result.TotalUs < result.TTFBUs || result.TTFBUs <= 0 {
		t.Fatalf("unexpected result %+v", result)
	}
	// 返回的数据量与请求不符（例如被劫持到别的页面）时不算成功
	if result = MeasureHTTP(context.Background(), "127.0.0.1", port, "n1", 2048); result.OK {
		t.Fatalf("short or long body accepted: %+v", result)
	}
	if result = MeasureHTTP(context.Background(), "127.0.0.1", port, "other", 4096); result.OK {
		t.Fatal("error page accepted")
	}
}

func TestMeasureRoundUsesOneConnection(t *testing.T) {
	var connections atomic.Int32
	var server = httptest.NewUnstartedServer(probeHandler(4096))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	var port = serverPort(server)
	var round = MeasureRound(context.Background(), "127.0.0.1", port, "n1", 3)
	if round.ConnectErr != nil || len(round.RTTs) != 3 || round.Failed != 0 || round.Signature != "sig2-1" || round.Client != "203.0.113.7" {
		t.Fatalf("round %+v", round)
	}
	if connections.Load() != 1 {
		t.Fatalf("round used %d connections", connections.Load())
	}

	// 透明代理：握手被代答，但没有节点的 HTTP 响应
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, io.LimitReader(conn, 64))
				_ = conn.Close()
			}()
		}
	}()
	round = MeasureRound(context.Background(), "127.0.0.1", listener.Addr().(*net.TCPAddr).Port, "n1", 2)
	if round.ConnectErr != nil || len(round.RTTs) != 0 || round.Failed != 2 || round.Signature != "" {
		t.Fatalf("intercepted connection measured as reachable: %+v", round)
	}

	// 响应不是拨测响应（大小不对）
	round = MeasureRound(context.Background(), "127.0.0.1", port, "wrong-nonce", 2)
	if len(round.RTTs) != 0 || round.Failed != 2 {
		t.Fatalf("error page counted as a round trip: %+v", round)
	}
}

func signedRelease(t *testing.T, binary []byte, version string, mutate func(*Manifest)) (*httptest.Server, ed25519.PublicKey) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var sum = sha256.Sum256(binary)
	var manifest = &Manifest{Version: version, Files: map[string]ManifestFile{Platform(): {Name: "edge-probe-" + Platform(), SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(binary))}}}
	if mutate != nil {
		mutate(manifest)
	}
	data, _ := json.Marshal(manifest)
	var sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
	var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/probe/manifest.json":
			_, _ = w.Write(data)
		case "/probe/manifest.json.sig":
			_, _ = w.Write([]byte(sig + "\n"))
		case "/probe/edge-probe-" + Platform():
			_, _ = w.Write(binary)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, pub
}

func TestUpdaterReplacesBinaryOnlyWithSignedManifest(t *testing.T) {
	var binary = []byte("new binary")
	var exe = filepath.Join(t.TempDir(), "edge-probe")
	if err := os.WriteFile(exe, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}

	server, pub := signedRelease(t, binary, "9.9.9", nil)
	var u = &Updater{Server: server.URL, HTTP: server.Client(), Key: pub, Executable: exe}
	if updated, err := u.Update(context.Background()); err != nil || !updated {
		t.Fatalf("updated=%v err=%v", updated, err)
	}
	if data, _ := os.ReadFile(exe); string(data) != "new binary" {
		t.Fatalf("binary not replaced: %q", data)
	}

	// 签名密钥不符
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	u.Key = other
	if _, err := u.Update(context.Background()); err == nil {
		t.Fatal("manifest signed by another key accepted")
	}

	// 清单签名正确，但下载的文件与清单不符
	_ = os.WriteFile(exe, []byte("old"), 0755)
	server, pub = signedRelease(t, binary, "9.9.9", func(m *Manifest) {
		var f = m.Files[Platform()]
		f.SHA256 = strings.Repeat("0", 64)
		m.Files[Platform()] = f
	})
	u = &Updater{Server: server.URL, HTTP: server.Client(), Key: pub, Executable: exe}
	if _, err := u.Update(context.Background()); err == nil {
		t.Fatal("binary with a different checksum accepted")
	}
	if data, _ := os.ReadFile(exe); string(data) != "old" {
		t.Fatal("binary replaced although verification failed")
	}

	// 同一版本不重复下载
	server, pub = signedRelease(t, binary, Version, nil)
	u = &Updater{Server: server.URL, HTTP: server.Client(), Key: pub, Executable: exe}
	if updated, err := u.Update(context.Background()); err != nil || updated {
		t.Fatalf("same version updated=%v err=%v", updated, err)
	}
}

func TestRunnerSyncsResultsWithPreviousNonce(t *testing.T) {
	_, probePort := probeServer(t, 1024)
	var mu sync.Mutex
	var requests []SyncRequest
	var platform = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/probe/sync" || r.Header.Get("Authorization") != "Bearer 7.s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(ErrorResponse{Error: "invalid agent"})
			return
		}
		var req SyncRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requests = append(requests, req)
		var n = len(requests)
		mu.Unlock()
		var nonce = "n1"
		if n > 1 {
			nonce = "n2"
		}
		_ = json.NewEncoder(w).Encode(SyncResponse{Nonce: nonce, HTTPBytes: 1024, Targets: []Target{{ID: 3, IP: "127.0.0.1", Port: probePort, HTTPPort: probePort}}, Resolve: []string{"g1.cdn.example.com", "missing.example.com"}})
	}))
	defer platform.Close()

	var dir = t.TempDir()
	var config = &Config{Server: platform.URL, AgentID: 7, Secret: "s3cret", MonthlyLimitMB: 10}
	var runner = NewRunner(config, filepath.Join(dir, "edge-probe.json"))
	runner.Updater = nil
	runner.LookupIP = func(_ context.Context, name string) ([]string, error) {
		if name == "g1.cdn.example.com" {
			return []string{"192.0.2.1"}, nil
		}
		return nil, errors.New("no such host")
	}
	if err := runner.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 新目标的首次 HTTP 拨测时间是随机错开的，这里让它立即到期
	runner.lastHTTP[3] = time.Time{}
	runner.ProbeOnce(context.Background())
	if err := runner.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || len(requests[0].Results) != 0 {
		t.Fatalf("unexpected requests %+v", requests)
	}
	var second = requests[1]
	if second.Nonce != "n1" || len(second.Results) != 1 {
		t.Fatalf("second sync %+v", second)
	}
	var result = second.Results[0]
	if result.TargetID != 3 || result.TCPSent != 1 || result.TCPOK != 1 || result.RTTSent != 2 || result.RTTOK != 2 || result.RTTMedianUs <= 0 || result.RTTSignature != "sig2-1" || result.RTTClient != "203.0.113.7" || result.HTTP == nil || !result.HTTP.OK || result.HTTP.Signature != "sig-1024" || result.HTTP.SignatureV2 != "sig2-1024" {
		t.Fatalf("unexpected result %+v http=%+v", result, result.HTTP)
	}
	// 第一次同步取得要解析的域名，第二次同步带上本机 DNS 的解析结果
	if len(requests[0].Resolves) != 0 || len(second.Resolves) != 2 || second.Resolves[0].Name != "g1.cdn.example.com" || len(second.Resolves[0].IPs) != 1 || second.Resolves[0].IPs[0] != "192.0.2.1" || second.Resolves[1].Error == "" {
		t.Fatalf("resolves %+v", second.Resolves)
	}
	if runner.Budget.Used() < 1024 {
		t.Fatal("probe traffic was not counted")
	}

	config.Secret = "wrong"
	if err := runner.SyncOnce(context.Background()); !IsUnauthorized(err) {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

func TestNetworkPathComparesPlatformAndNodeIPs(t *testing.T) {
	for _, c := range []struct {
		platform string
		nodes    []string
		clean    bool
		want     string
	}{
		{"203.0.113.7", []string{"203.0.113.7"}, false, "direct 203.0.113.7"},
		{"203.0.113.7", []string{"203.0.113.9"}, false, "direct 203.0.113.7"},
		{"203.0.113.7", []string{"203.0.113.7", "198.51.100.2"}, true, "proxied platform=203.0.113.7 node=198.51.100.2"},
		{"203.0.113.7", nil, true, "unknown"},
		{"", []string{"203.0.113.7"}, false, "unknown"},
		// 平台的请求走代理，拨测直连：以节点看到的 IP 为准
		{"198.51.100.10", []string{"192.0.2.20", "192.0.2.20"}, true, "direct 192.0.2.20"},
		// 握手没有到达节点，或数据不足，不能这样判断
		{"198.51.100.10", []string{"192.0.2.20"}, false, "proxied platform=198.51.100.10 node=192.0.2.20"},
	} {
		if got := networkPath(c.platform, c.nodes, c.clean); got != c.want {
			t.Fatalf("networkPath(%q, %v, %v) = %q, want %q", c.platform, c.nodes, c.clean, got, c.want)
		}
	}
}

func TestHandshakeInterceptedNeedsMostNodes(t *testing.T) {
	var result = func(tcp, rtt int64) Result {
		return Result{TCPOK: 1, RTTOK: 2, TCPMedianUs: tcp, RTTMedianUs: rtt}
	}
	// 经路由器透明代理实测的数值
	var proxied = []Result{result(1500, 323700), result(2000, 354100), result(1600, 88200), result(3900, 94700)}
	if !handshakeIntercepted(proxied) {
		t.Fatal("handshakes answered by a local proxy were not detected")
	}
	for name, results := range map[string][]Result{
		"direct":          {result(30000, 34000), result(180000, 190000), result(5000, 6000)},
		"one busy node":   {result(30000, 34000), result(30000, 400000), result(30000, 33000)},
		"nearby node":     {result(800, 4000), result(900, 5000), result(700, 3000)},
		"too few samples": {result(1500, 323700)},
	} {
		if handshakeIntercepted(results) {
			t.Fatalf("%s must not count as proxied", name)
		}
	}
}

func TestPlatformIgnoresProxyEnvironment(t *testing.T) {
	if ipv4Transport().Proxy != nil {
		t.Fatal("platform requests must take the same path as the probes")
	}
}

func TestBudgetStopsAtLimitAndResetsMonthly(t *testing.T) {
	var path = filepath.Join(t.TempDir(), "budget")
	var now = time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	var b = NewBudget(path, 1)
	b.now = func() time.Time { return now }
	if !b.Allow(1<<20 - 100) {
		t.Fatal("within limit rejected")
	}
	if b.Allow(200) {
		t.Fatal("over limit allowed")
	}
	b.Save()
	var restored = NewBudget(path, 1)
	restored.now = b.now
	if restored.Used() != 1<<20-100 {
		t.Fatalf("usage not persisted: %d", restored.Used())
	}
	now = now.Add(2 * time.Hour)
	if !restored.Allow(200) {
		t.Fatal("budget did not reset in the new month")
	}
}

func TestValidateServerRequiresHTTPS(t *testing.T) {
	for server, ok := range map[string]bool{"https://example.com": true, "http://127.0.0.1:8080": true, "http://example.com": false, "example.com": false} {
		if (ValidateServer(server) == nil) != ok {
			t.Fatalf("%s: expected ok=%v", server, ok)
		}
	}
}
