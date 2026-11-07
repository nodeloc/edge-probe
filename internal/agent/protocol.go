package agent

// 与 EdgeUser /api/probe/* 之间的 JSON 协议。字段只增不改，旧版本 agent 忽略不认识的字段。

type BindRequest struct {
	Token   string `json:"token"`
	Name    string `json:"name"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

type BindResponse struct {
	AgentID int64  `json:"agentId"`
	Secret  string `json:"secret"`
}

type SyncRequest struct {
	Version  string    `json:"version"`
	OS       string    `json:"os"`
	Arch     string    `json:"arch"`
	Nonce    string    `json:"nonce"` // 本次结果中 HTTP 拨测使用的 nonce
	Results  []Result  `json:"results"`
	Resolves []Resolve `json:"resolves,omitempty"` // 用本机的 DNS 解析服务端指定的域名
}

// Resolve 本机网络（运营商或路由器的 DNS）解析一个域名的结果
type Resolve struct {
	Name       string   `json:"name"`
	IPs        []string `json:"ips"`
	Error      string   `json:"error,omitempty"`
	DurationUs int64    `json:"durationUs"`
}

// Result 一个目标在一个上报周期内的汇总
type Result struct {
	TargetID    int64  `json:"targetId"`
	IP          string `json:"ip"`
	TCPSent     int    `json:"tcpSent"`
	TCPOK       int    `json:"tcpOk"`
	TCPMedianUs int64  `json:"tcpMedianUs"`
	TCPP90Us    int64  `json:"tcpP90Us"`
	TCPMinUs    int64  `json:"tcpMinUs"`
	// 在已建立的连接上发送小请求测得的往返时间；响应由节点签名，透明代理无法伪造
	RTTSent      int         `json:"rttSent"`
	RTTOK        int         `json:"rttOk"`
	RTTMedianUs  int64       `json:"rttMedianUs"`
	RTTP90Us     int64       `json:"rttP90Us"`
	RTTMinUs     int64       `json:"rttMinUs"`
	RTTSignature string      `json:"rttSignature"` // 节点对 nonce、大小和它看到的客户端 IP 的签名（v2）
	RTTClient    string      `json:"rttClient"`    // 节点看到的客户端 IP
	HTTP         *HTTPResult `json:"http,omitempty"`
}

type HTTPResult struct {
	OK        bool   `json:"ok"`
	TTFBUs    int64  `json:"ttfbUs"`
	TotalUs   int64  `json:"totalUs"`
	Bytes     int64  `json:"bytes"`
	Signature string `json:"signature"`
	// v2：签名同时覆盖节点看到的客户端 IP
	SignatureV2 string `json:"signatureV2"`
	Client      string `json:"client"`
}

type SyncResponse struct {
	Nonce         string   `json:"nonce"`
	SyncSeconds   int      `json:"syncSeconds"`
	TCPSeconds    int      `json:"tcpSeconds"`
	HTTPSeconds   int      `json:"httpSeconds"`
	HTTPBytes     int      `json:"httpBytes"`
	Targets       []Target `json:"targets"`
	LatestVersion string   `json:"latestVersion"`
	Resolve       []string `json:"resolve"`  // 需要用本机 DNS 解析的域名（集群域名）
	ClientIP      string   `json:"clientIp"` // 平台看到的 agent IP，doctor 用来判断是否直连
}

type Target struct {
	ID       int64  `json:"id"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	HTTPPort int    `json:"httpPort"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// ProbePath 节点上的拨测地址，只在以 IP 访问时生效
const ProbePath = "/.well-known/nodepend-probe"

// RTTProbeBytes 往返测量请求的响应大小
const RTTProbeBytes = 1
