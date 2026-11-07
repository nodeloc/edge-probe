package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Server  string `json:"server"`
	AgentID int64  `json:"agentId"`
	Secret  string `json:"secret"`
	Name    string `json:"name"`
	// MonthlyLimitMB 每月拨测流量上限（估算值），达到后暂停拨测到下个月
	MonthlyLimitMB int64 `json:"monthlyLimitMB"`
}

const DefaultMonthlyLimitMB = 1024

// DefaultConfigPath 配置默认与程序放在同一目录，安装、升级和卸载都只涉及这一个目录
func DefaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "edge-probe.json"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "edge-probe.json")
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config = &Config{}
	if err = json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if config.AgentID <= 0 || config.Secret == "" {
		return nil, fmt.Errorf("%s is not bound yet; run: edge-probe bind --server URL --token TOKEN", path)
	}
	if err = ValidateServer(config.Server); err != nil {
		return nil, err
	}
	if config.MonthlyLimitMB <= 0 {
		config.MonthlyLimitMB = DefaultMonthlyLimitMB
	}
	return config, nil
}

// SaveConfig 先写临时文件再改名，中途断电不会留下半个配置
func SaveConfig(path string, config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	var tmp = path + ".tmp"
	if err = os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ValidateServer 只接受 https；本机地址允许 http 便于调试
func ValidateServer(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return errors.New("invalid server URL: " + server)
	}
	if u.Scheme == "https" {
		return nil
	}
	var host = u.Hostname()
	if u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	return errors.New("server URL must use https: " + server)
}

func serverURL(server, path string) string {
	return strings.TrimRight(server, "/") + path
}
