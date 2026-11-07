#!/bin/sh
# 把 edge-probe 拨测的节点 IP 同步到路由器代理的直连列表：只放行这些地址，其他流量照常走代理。
# 节点的 IP 会变化，而 HomeProxy 的直连列表不支持 URL，所以由这个脚本定期读取后写回。
# 不放行平台：平台在 Cloudflare 后面，控制台共用这些 IP，直连后局域网里打开控制台会非常慢。
# 在装有 HomeProxy 的 OpenWrt 路由器上以 root 执行：
#   wget -qO- https://example.com/probe/router-direct.sh | sh -s -- --server https://example.com
# 卸载：
#   wget -qO- https://example.com/probe/router-direct.sh | sh -s -- --uninstall
set -eu

BIN=/usr/bin/edge-probe-direct
STATE=/etc/edge-probe-direct.list
CRON=/etc/crontabs/root
OPTION=homeproxy.control.wan_direct_ipv4_ips
MAX_IPS=256
SERVER=""
MODE=install
QUIET=0

fail() {
	echo "error: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--server) SERVER="${2:-}"; shift 2 ;;
	--sync) MODE=sync; shift ;;
	--quiet) QUIET=1; shift ;;
	--uninstall) MODE=uninstall; shift ;;
	*) fail "unknown option $1" ;;
	esac
done

[ "$(id -u)" = 0 ] || fail "please run as root"
command -v uci >/dev/null 2>&1 || fail "this script runs on an OpenWrt router"
uci -q get homeproxy.control >/dev/null || fail "HomeProxy is not installed on this router"

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 2 -m 30 "$1"
	else
		wget -q -T 30 -O- "$1"
	fi
}

# 只接受公网 IPv4，整行匹配。任何一行不符合都放弃这次更新，错误页不会被当成列表写入
public_ips() {
	awk -F. '/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ && $1>0 && $1<224 && $1!=10 && $1!=127 && $2<=255 && $3<=255 && $4<=255 &&
		!($1==100 && $2>=64 && $2<=127) && !($1==169 && $2==254) && !($1==172 && $2>=16 && $2<=31) && !($1==192 && $2==168)'
}

current_ips() {
	uci -q get "$OPTION" | tr ' ' '\n' | sed '/^$/d' || true
}

# 脚本加入的地址记录在 $STATE，用户自己填写的地址保持不动
manual_ips() {
	if [ -f "$STATE" ]; then
		current_ips | grep -vxF -f "$STATE" || true
	else
		current_ips
	fi
}

write_ips() {
	uci -q delete "$OPTION" || true
	for ip in $1; do
		uci add_list "$OPTION=$ip"
	done
	uci commit homeproxy
	if /etc/init.d/homeproxy running; then
		/etc/init.d/homeproxy restart
	fi
}

sync_ips() {
	case "$SERVER" in https://*) ;; *) fail "--server must start with https://" ;; esac

	NODES="$(fetch "$SERVER/api/probe/direct-ips")" || fail "cannot read the node list, keeping the current settings"
	[ -n "$NODES" ] || fail "the node list is empty, keeping the current settings"

	VALID="$(printf '%s\n' "$NODES" | public_ips)"
	# 全部无效时 VALID 为空，printf 仍然输出一行，只比较行数会放过单行的错误页并清空列表
	[ -n "$VALID" ] && [ "$(printf '%s\n' "$NODES" | wc -l)" = "$(printf '%s\n' "$VALID" | wc -l)" ] ||
		fail "the node list has invalid lines, keeping the current settings"
	WANTED="$(printf '%s\n' "$VALID" | sort -u)"
	[ "$(printf '%s\n' "$WANTED" | wc -l)" -le "$MAX_IPS" ] || fail "the node list is too long, keeping the current settings"

	# 列表没有变化时不写配置也不重启，避免定时任务打断代理
	if [ -f "$STATE" ] && [ "$WANTED" = "$(cat "$STATE")" ] && [ -z "$(printf '%s\n' "$WANTED" | grep -vxF "$(current_ips)" || true)" ]; then
		return 0
	fi
	MANUAL="$(manual_ips)"
	printf '%s\n' "$WANTED" >"$STATE"
	write_ips "$(printf '%s\n%s\n' "$MANUAL" "$WANTED" | sed '/^$/d' | sort -u)"
	echo "updated: $(printf '%s\n' "$WANTED" | wc -l) direct addresses"
}

case "$MODE" in
sync)
	sync_ips
	;;
uninstall)
	sed -i "\|$BIN|d" "$CRON" 2>/dev/null || true
	/etc/init.d/cron restart 2>/dev/null || true
	if [ -f "$STATE" ]; then
		write_ips "$(manual_ips)"
	fi
	rm -f "$BIN" "$STATE"
	echo "removed. Addresses you added yourself were kept."
	;;
install)
	[ -n "$SERVER" ] || fail "--server is required"
	case "$SERVER" in https://*) ;; *) fail "--server must start with https://" ;; esac
	SERVER="${SERVER%/}"
	fetch "$SERVER/probe/router-direct.sh" >"$BIN.new" || fail "cannot download the script"
	chmod 755 "$BIN.new"
	mv -f "$BIN.new" "$BIN"
	"$BIN" --sync --server "$SERVER"
	# 错开分钟，避免所有路由器同时请求
	MINUTE=$(($(date +%s) % 30))
	touch "$CRON"
	sed -i "\|$BIN|d" "$CRON"
	echo "$MINUTE,$((MINUTE + 30)) * * * * $BIN --sync --server $SERVER" >>"$CRON"
	/etc/init.d/cron enable
	/etc/init.d/cron restart
	echo "完成：edge-probe 拨测的节点 IP 已设为直连，每 30 分钟自动更新，其他流量照常走代理。"
	[ "$QUIET" = 1 ] || echo "请在运行 edge-probe 的设备上重新执行安装命令确认结果。"
	;;
esac
