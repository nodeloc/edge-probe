package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// APIError 服务端拒绝请求；Status 为 401/403 时表示 agent 已被删除、停用或封禁
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.Status, e.Message)
}

func IsUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)
}

type Client struct {
	Server string
	HTTP   *http.Client
}

func NewClient(server string) *Client {
	return &Client{Server: server, HTTP: &http.Client{Timeout: 30 * time.Second, Transport: ipv4Transport()}}
}

// ipv4Transport 与平台之间只用 IPv4：拨测目标是节点的 IPv4 地址，平台据同步时的 IPv4 判断地区，
// 并与节点看到的 IP 比较；双栈网络下若同步走 IPv6，两者永远不同。
// 同样的原因，这里不读取 HTTP_PROXY：拨测不走环境变量代理，同步走了会被误判为经代理
func ipv4Transport() *http.Transport {
	var transport = http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialPlatform(ctx, addr)
	}
	return transport
}

func (c *Client) Bind(ctx context.Context, req *BindRequest) (*BindResponse, error) {
	var resp = &BindResponse{}
	if err := c.post(ctx, "/api/probe/bind", "", req, resp); err != nil {
		return nil, err
	}
	if resp.AgentID <= 0 || resp.Secret == "" {
		return nil, errors.New("server returned an incomplete binding")
	}
	return resp, nil
}

func (c *Client) Sync(ctx context.Context, config *Config, req *SyncRequest) (*SyncResponse, error) {
	var resp = &SyncResponse{}
	var auth = "Bearer " + strconv.FormatInt(config.AgentID, 10) + "." + config.Secret
	if err := c.post(ctx, "/api/probe/sync", auth, req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) post(ctx context.Context, path, auth string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL(c.Server, path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "edge-probe/"+Version)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e ErrorResponse
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	return json.Unmarshal(data, out)
}
