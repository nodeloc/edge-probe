# edge-probe

edge-probe 是 [NodePend](https://nodepend.com) 的拨测程序。它运行在用户自己的网络里，测量到各加速节点的延迟，平台据此为不同地区和运营商的访客选择最快的节点。

这个仓库公开 edge-probe 的完整源码和安装脚本，方便你在安装前审查它做了什么，或者自己编译。平台分发的程序就是由这份源码编译的，可以按下文的方法核对。

*English: edge-probe is the latency probe agent for NodePend. It runs on your own network, measures latency to edge nodes, and reports the results so the platform can route visitors to the fastest node. This repository holds its full source and install scripts so you can audit or build it yourself.*

## 它做什么

edge-probe 只做以下几件事：

- 每 5 分钟向平台同步一次，领取要测量的节点列表，并上报汇总结果。
- 每分钟对每个节点发起 TCP 连接和几个字节的小请求，测量往返延迟。节点会对响应签名，平台只采信签名通过的结果。
- 每小时对每个节点下载一个小文件（默认 32 KB，最大 64 KB），测量下载速度。
- 用本机 DNS 解析平台指定的少量域名（最多 8 个），用来判断本地解析到了哪些节点。

它不会读取设备上的任何文件或数据，不监听端口，也不执行平台下发的命令。唯一会被替换的是它自己：见下文「自动升级」。

默认每月拨测流量不超过 1 GB（估算值）。达到上限后暂停，下个月继续。上限可以在绑定时用 `--monthly-limit-mb` 调整，用量记在配置文件旁边的 `edge-probe.json.budget` 中。

## 安装

在控制台的「拨测」页面获取绑定令牌，然后按页面上的命令安装。

Linux 和路由器上可以使用安装脚本，脚本源码在 [scripts/install.sh](scripts/install.sh)：

```sh
curl -fsSL https://nodepend.com/probe/install.sh | sh -s -- --server https://nodepend.com --token <TOKEN>
```

其他系统下载对应平台的程序后执行：

```sh
edge-probe bind --server https://nodepend.com --token <TOKEN>
edge-probe run
```

## 命令

| 命令 | 说明 |
| --- | --- |
| `edge-probe bind --server URL --token TOKEN [--name NAME] [--monthly-limit-mb N] [--config PATH]` | 用控制台的令牌绑定，生成配置文件 |
| `edge-probe run [--no-direct] [--config PATH]` | 持续运行拨测 |
| `edge-probe doctor [--no-direct] [--config PATH]` | 检查与平台的连接，执行一轮拨测并输出结果，包括是否直连 |
| `edge-probe version` | 显示版本 |

配置文件默认是程序所在目录下的 `edge-probe.json`，权限为 0600，里面保存平台地址、agent ID 和密钥。平台地址只接受 https，调试时可以用本机的 http 地址。

## 代理环境

如果设备上运行着 sing-box、Xray、Clash 等 TUN 模式的代理，edge-probe 默认会把连接绑定到默认路由的网卡上，绕过代理直连，其他程序不受影响。测出的延迟要反映你真实的网络，所以经过代理的结果不会计入。不需要绕过时，使用 `--no-direct`。

路由器上的透明代理（目前支持 HomeProxy）可以使用 [scripts/router-direct.sh](scripts/router-direct.sh)，它只放行 edge-probe 要连接的节点地址，其他流量照常走代理。

## 自动升级

edge-probe 会定期从平台获取 `/probe/manifest.json` 和它的签名 `/probe/manifest.json.sig`。清单必须通过 Ed25519 签名校验，下载的程序也必须与清单中的 SHA-256 和大小一致，否则一律拒绝。

签名公钥写在 [internal/agent/update.go](internal/agent/update.go) 中。私钥只保存在发布机器上，不在平台服务器上，因此即使服务器被入侵，也无法向你的设备推送程序。

## 自己编译

需要 Go 1.22 或更高版本，没有第三方依赖：

```sh
go build -o edge-probe ./cmd/edge-probe
```

## 核对平台分发的程序

平台的发布构建是可复现的。用相同的 Go 版本和参数编译，就能得到与平台分发的程序完全相同的文件。

发布构建使用 Go 1.24.13（Docker 镜像 `golang@sha256:d2d2bc1c84f7e60d7d2438a3836ae7d0c847f4888464e7ec9ba3a1339a1ee804`）和以下参数：

```sh
docker run --rm --platform linux/amd64 -v "$PWD":/src:ro -v "$PWD/dist":/out -w /src \
  -e GOTOOLCHAIN=local -e CGO_ENABLED=0 -e GOARM=7 -e GOMIPS=softfloat \
  golang@sha256:d2d2bc1c84f7e60d7d2438a3836ae7d0c847f4888464e7ec9ba3a1339a1ee804 \
  sh -c 'GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/edge-probe-linux-amd64 ./cmd/edge-probe'
sha256sum dist/edge-probe-linux-amd64
```

然后把结果与平台清单中的值比较：

```sh
curl -fsSL https://nodepend.com/probe/manifest.json
```

清单中的 `version` 对应 [internal/agent/update.go](internal/agent/update.go) 里的 `Version`。请检出与之相同的版本再编译。

支持的平台：`linux-amd64`、`linux-arm64`、`linux-arm`、`linux-mips`、`linux-mipsle`、`windows-amd64`、`darwin-amd64`、`darwin-arm64`。

## 许可证

[MIT](LICENSE)
