#!/bin/sh
# edge-probe 安装脚本（Linux / OpenWrt），以 root 执行：
#   curl -fsSL https://example.com/probe/install.sh | sh -s -- --server https://example.com --token TOKEN [--name NAME]
# edge-probe 默认绕过本机的 TUN 代理走默认网络，安装结束时检查是否直连；--no-direct 关闭绕过。
# 装在有 HomeProxy 的 OpenWrt 路由器上时，同时安装 router-direct.sh，让节点 IP 保持直连
# 卸载：
#   curl -fsSL https://example.com/probe/install.sh | sh -s -- --uninstall
set -eu

DIR=/opt/edge-probe
SERVER=""
TOKEN=""
NAME=""
UNINSTALL=0
REBIND=0
DIRECT=1

fail() {
	echo "error: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--server) SERVER="${2:-}"; shift 2 ;;
	--token) TOKEN="${2:-}"; shift 2 ;;
	--name) NAME="${2:-}"; shift 2 ;;
	--rebind) REBIND=1; shift ;;
	--no-direct) DIRECT=0; shift ;;
	--uninstall) UNINSTALL=1; shift ;;
	*) fail "unknown option $1" ;;
	esac
done

[ "$(id -u)" = 0 ] || fail "please run as root"
[ "$(uname -s)" = Linux ] || fail "this script supports Linux only; download the program from the console for other systems"

has() { command -v "$1" >/dev/null 2>&1; }
is_systemd() { has systemctl && [ -d /run/systemd/system ]; }
is_procd() { [ -f /etc/rc.common ] && [ -e /sbin/procd ]; }

if [ "$UNINSTALL" = 1 ]; then
	if is_systemd; then
		systemctl disable --now edge-probe 2>/dev/null || true
		rm -f /etc/systemd/system/edge-probe.service
		systemctl daemon-reload
	elif is_procd; then
		/etc/init.d/edge-probe stop 2>/dev/null || true
		/etc/init.d/edge-probe disable 2>/dev/null || true
		rm -f /etc/init.d/edge-probe
		if [ -x /usr/bin/edge-probe-direct ]; then
			/usr/bin/edge-probe-direct --uninstall || true
		fi
	fi
	rm -rf "$DIR"
	echo "edge-probe removed. Delete it in the console as well."
	exit 0
fi

[ -n "$SERVER" ] || fail "--server is required"
case "$SERVER" in https://*) ;; *) fail "--server must start with https://" ;; esac
SERVER="${SERVER%/}"

case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
armv7* | armv8l | armhf) ARCH=arm ;;
mips*)
	# 路由器的 MIPS 有大端和小端两种
	if [ "$(printf '\001\000' | od -An -tu2 | tr -d ' ')" = 1 ]; then ARCH=mipsle; else ARCH=mips; fi
	;;
*) fail "unsupported CPU architecture $(uname -m)" ;;
esac

download() {
	if has curl; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif has wget; then
		wget -q -O "$2" "$1"
	else
		fail "curl or wget is required"
	fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FILE="edge-probe-linux-$ARCH"
echo "downloading $FILE ..."
download "$SERVER/probe/$FILE" "$TMP/edge-probe"
download "$SERVER/probe/manifest.json" "$TMP/manifest.json"
EXPECTED="$(tr -d '\n ' <"$TMP/manifest.json" | sed -n "s/.*\"linux-$ARCH\":{[^}]*\"sha256\":\"\([0-9a-f]*\)\".*/\1/p")"
if has sha256sum && [ -n "$EXPECTED" ]; then
	ACTUAL="$(sha256sum "$TMP/edge-probe" | cut -d' ' -f1)"
	[ "$ACTUAL" = "$EXPECTED" ] || fail "checksum mismatch, please retry"
fi
chmod 755 "$TMP/edge-probe"
"$TMP/edge-probe" version >/dev/null || fail "the downloaded program cannot run on this device"

mkdir -p "$DIR"
mv -f "$TMP/edge-probe" "$DIR/edge-probe"

if [ ! -f "$DIR/edge-probe.json" ] || [ "$REBIND" = 1 ]; then
	[ -n "$TOKEN" ] || fail "--token is required"
	if [ -n "$NAME" ]; then
		"$DIR/edge-probe" bind --server "$SERVER" --token "$TOKEN" --name "$NAME"
	else
		"$DIR/edge-probe" bind --server "$SERVER" --token "$TOKEN"
	fi
else
	echo "already bound, keeping $DIR/edge-probe.json (use --rebind to bind again)"
fi

RUN_USER=root
NETWORK=""
RUN_ARGS=""
[ "$DIRECT" = 1 ] || RUN_ARGS=" --no-direct"

# 路由器上的 HomeProxy 接管 edge-probe 的流量时，把节点 IP 设为直连。放在单独的脚本里而不是程序里：
# 程序自动升级，节点列表没有签名，不应该由它以 root 修改代理配置。失败时不影响安装
setup_router_direct() {
	[ "$DIRECT" = 1 ] || return 0
	has uci && uci -q get homeproxy.control >/dev/null || return 0
	echo "HomeProxy found, setting the node addresses to direct ..."
	if ! (download "$SERVER/probe/router-direct.sh" "$TMP/router-direct.sh" && sh "$TMP/router-direct.sh" --server "$SERVER" --quiet); then
		echo "could not update HomeProxy, run this later:"
		echo "  wget -qO- $SERVER/probe/router-direct.sh | sh -s -- --server $SERVER"
	fi
}

# 以服务的身份运行，结果与服务实际走的路径一致
run_as_service() {
	if [ "$RUN_USER" = root ]; then
		"$@"
	elif has runuser; then
		runuser -u "$RUN_USER" -- "$@"
	else
		su -s /bin/sh "$RUN_USER" -c "$*"
	fi
}

doctor_network() {
	OUTPUT="$(run_as_service "$DIR/edge-probe" doctor "$@" 2>&1 </dev/null)" || return 1
	printf '%s\n' "$OUTPUT" | sed -n 's/^network: //p'
}

check_network() {
	# shellcheck disable=SC2086
	NETWORK="$(doctor_network $RUN_ARGS)" || NETWORK=""
}

# 不绕过时的出口不同，说明绕过确实生效了；相同时无法区分，可能是全部流量都走了同一个代理
bypass_proved() {
	[ "$DIRECT" = 1 ] || return 1
	PROXIED="$(doctor_network --no-direct)" || return 1
	case "$PROXIED" in
	direct*) [ "$PROXIED" != "$NETWORK" ] ;;
	proxied*) return 0 ;;
	*) return 1 ;;
	esac
}

is_running() {
	if has pidof; then pidof "$1" >/dev/null 2>&1; else pgrep -x "$1" >/dev/null 2>&1; fi
}

has_local_proxy() {
	for name in sing-box xray v2ray mihomo clash; do
		is_running "$name" && return 0
	done
	return 1
}

print_iptables_fix() {
	echo "执行下面的命令，放行 edge-probe 用户的流量（重启后失效，请同时写入防火墙配置）："
	echo "  iptables -t nat -I OUTPUT -m owner --uid-owner $1 -j RETURN"
	echo "  iptables -t mangle -I OUTPUT -m owner --uid-owner $1 -j RETURN"
	echo "使用 nftables 时，在透明代理的 output 链最前面加入：meta skuid $1 return"
}

# 只给出让流量不进代理的方法：代理内的进程规则（process、PROCESS-NAME）仍由代理代答握手，平台照样判定为经代理
print_local_proxy_fix() {
	if [ "$RUN_USER" = root ]; then
		# OpenWrt 上以 root 运行，没有专用用户，只能按目标地址放行
		if has uci && uci -q get homeproxy.control >/dev/null; then
			echo "这台路由器装有 HomeProxy，请执行下面的命令，只放行 edge-probe 连接的地址："
			echo "  wget -qO- $SERVER/probe/router-direct.sh | sh -s -- --server $SERVER"
		else
			echo "edge-probe 以 root 运行，无法按用户放行。请把下面地址中的节点 IP 和平台的 IP 加入防火墙或路由层的直连列表："
			echo "  $SERVER/api/probe/direct-ips"
			echo "不要用代理内的分流规则，那样流量仍然经过代理。"
		fi
	elif is_running sing-box; then
		echo "本机正在运行 sing-box，请在 tun 入站中加入下面的配置后重启它："
		echo "  \"exclude_uid\": [$(id -u "$RUN_USER")]"
	elif is_running mihomo || is_running clash; then
		echo "本机正在运行 mihomo/Clash。使用 TUN 时，在 tun 配置中加入下面的内容后重启它："
		echo "  exclude-uid:"
		echo "    - $(id -u "$RUN_USER")"
		echo "没有使用 TUN 时："
		print_iptables_fix "$(id -u "$RUN_USER")"
	else
		echo "本机正在运行 Xray。"
		print_iptables_fix "$(id -u "$RUN_USER")"
	fi
	echo "改完后重新执行：curl -fsSL $SERVER/probe/install.sh | sh -s -- --server $SERVER"
}

# 在 nat 链重定向的流量（REDIRECT、sing-box 的 auto_redirect）和路由器上的代理绕不开，需要放行
print_proxy_help() {
	echo
	echo "检测到 edge-probe 的流量仍然经过代理：测到的是代理线路的延迟，这台设备的数据不会参与调度。"
	if has_local_proxy; then
		print_local_proxy_fix
	else
		LOCAL_IP="$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')"
		echo "本机没有发现代理程序，流量通常是被路由器或旁路由接管了。"
		echo "请在路由器的代理设置中把这台设备${LOCAL_IP:+（$LOCAL_IP）}设为直连，然后重新执行："
		echo "  curl -fsSL $SERVER/probe/install.sh | sh -s -- --server $SERVER"
		echo "路由器用的是 HomeProxy 时，也可以在路由器上执行下面的命令，只放行 edge-probe 连接的地址："
		echo "  wget -qO- $SERVER/probe/router-direct.sh | sh -s -- --server $SERVER"
	fi
}

# 全部流量都走同一个代理时，平台和节点看到的是同一个出口，只看这一次检查无法与直连区分
report_network() {
	case "$NETWORK" in
	direct*)
		echo "网络检查通过：节点看到的出口 IP 是 ${NETWORK#direct }。"
		if has_local_proxy; then
			if bypass_proved; then
				echo "已自动绕过本机的代理，不需要修改代理配置。"
			else
				echo
				echo "请确认这是你宽带的公网 IP。如果它是代理服务器的 IP，说明流量仍然经过代理。"
				print_local_proxy_fix
			fi
		fi
		;;
	proxied*) print_proxy_help ;;
	*) echo "暂时无法判断是否直连，可以在控制台查看这台设备是否显示“经代理”。" ;;
	esac
}

if is_systemd; then
	if id edge-probe >/dev/null 2>&1 || (has useradd && useradd -r -M -d "$DIR" -s /usr/sbin/nologin edge-probe 2>/dev/null); then
		RUN_USER=edge-probe
		chown -R edge-probe "$DIR"
	fi
	cat >/etc/systemd/system/edge-probe.service <<EOF
[Unit]
Description=edge-probe network latency probe
After=network-online.target
Wants=network-online.target

[Service]
User=$RUN_USER
ExecStart=$DIR/edge-probe run$RUN_ARGS
Restart=always
RestartSec=10
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DIR
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
	systemctl enable edge-probe >/dev/null 2>&1
	systemctl restart edge-probe
	echo "edge-probe is running. Logs: journalctl -u edge-probe -f"
	echo "checking the network, this can take up to a minute ..."
	check_network || true
	report_network
elif is_procd; then
	cat >/etc/init.d/edge-probe <<EOF
#!/bin/sh /etc/rc.common
START=99
USE_PROCD=1

start_service() {
	procd_open_instance
	procd_set_param command $DIR/edge-probe run$RUN_ARGS
	procd_set_param respawn 3600 10 0
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}
EOF
	chmod 755 /etc/init.d/edge-probe
	/etc/init.d/edge-probe enable
	# 首次安装时服务尚未注册，restart 中的 stop 会让 ubus 输出 “Command failed: Not found”
	/etc/init.d/edge-probe stop >/dev/null 2>&1 || true
	/etc/init.d/edge-probe start
	echo "edge-probe is running. Logs: logread -e edge-probe"
	setup_router_direct
	echo "checking the network, this can take up to a minute ..."
	check_network || true
	report_network
else
	echo "no systemd or procd found; start it manually and keep it running:"
	echo "  nohup $DIR/edge-probe run >$DIR/edge-probe.log 2>&1 &"
fi
