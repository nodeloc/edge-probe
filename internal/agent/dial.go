package agent

import (
	"context"
	"net"
	"sync/atomic"
	"time"
)

// directDial 连接绑定到默认路由的网卡，绕过本机 TUN 代理的策略路由。放在程序里而不是服务配置里，
// 这样已安装的设备随自动升级生效，也不需要 root 权限
var directDial atomic.Bool

// bindRetryAt 绑定后连不上平台时暂停绑定的截止时间。一次网络波动不应该让设备永远放弃绕过
var bindRetryAt atomic.Int64

const bindRetrySeconds = 3600

func bindEnabled() bool {
	return directDial.Load() && time.Now().Unix() >= bindRetryAt.Load()
}

// SetDirect 由 run 和 doctor 根据 --no-direct 设置
func SetDirect(on bool) {
	directDial.Store(on)
}

func probeDialer(timeout time.Duration) *net.Dialer {
	var dialer = &net.Dialer{Timeout: timeout}
	if bindEnabled() {
		dialer.Control = bindDefaultInterface
	}
	return dialer
}

// dialPlatform 连接平台。代理用 fake-ip 接管 DNS 时，平台的域名只有经过代理才能连上；
// 这时放弃绑定，拨测也走同一条路径，否则平台和节点看到的 IP 不同
func dialPlatform(ctx context.Context, addr string) (net.Conn, error) {
	var bound = bindEnabled()
	if bound {
		conn, err := probeDialer(5*time.Second).DialContext(ctx, "tcp4", addr)
		if err == nil {
			return conn, nil
		}
	}
	var dialer = &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp4", addr)
	if err == nil && bound {
		bindRetryAt.Store(time.Now().Unix() + bindRetrySeconds)
	}
	return conn, err
}
