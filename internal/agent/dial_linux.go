package agent

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// bindDefaultInterface 绑定失败（5.7 以下的内核需要特权）时不影响连接，只是走系统默认的路径
func bindDefaultInterface(_, _ string, c syscall.RawConn) error {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	var name = defaultInterface(string(data))
	if name == "" {
		return nil
	}
	return c.Control(func(fd uintptr) {
		_ = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, name)
	})
}

// defaultInterface 从 /proc/net/route 取默认路由的网卡。这个文件只包含 main 路由表，
// TUN 代理写在单独路由表里的默认路由不会出现在这里
func defaultInterface(routes string) string {
	var best = ""
	var bestMetric = 0
	for _, line := range strings.Split(routes, "\n") {
		var fields = strings.Fields(line)
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&1 == 0 {
			continue
		}
		metric, err := strconv.Atoi(fields[6])
		if err != nil {
			continue
		}
		if best == "" || metric < bestMetric {
			best, bestMetric = fields[0], metric
		}
	}
	return best
}
