# SkyHorse（天马）第四代

天马是一个基于 **原生 QUIC/UDP** 的代理协议。第四代不再在 QUIC 之上叠加 smux，而是直接使用 QUIC 原生多流：

```
QUIC v1 + TLS 1.3
├── QUIC 双向流：SOCKS5 TCP CONNECT
└── QUIC DATAGRAM：SOCKS5 UDP
```

外层可用 Salamander 混淆轮换 + 前置门，隐藏 QUIC 指纹并抗主动探测。

> 天马不与 Hysteria 2 互通，也不是它的分支。

## 第四代的变化

第三代为了把连接管理交给内核，在 QUIC 之上又套了一层 sing-mux，结果是双层复用：

- 每条流多一次 smux 封装、窗口和调度；
- 单流吞吐被 smux 的默认窗口卡住；
- 端口轮换依赖关闭 mux 会话，行为不直观。

第四代回退为 **QUIC 原生多流**，保留内核生命周期管理，但不再引入额外复用层。

| 能力 | 第三代 | 第四代 |
|---|---|---|
| 数据面复用 | QUIC + smux 双层 | QUIC 原生多流，单层 |
| 单流吞吐 | 受 smux 窗口限制 | 直接受 QUIC 流控与拥塞控制 |
| 内核职责 | Start/Close + mux 会话 | Start/Close + 连接生命周期 |
| 并发限制 | mux `max_streams` | 客户端流信号量，按 `max_streams_per_connection` 等待 |
| 拥塞控制 | 固定/默认 | BBR 可选档位，或 Brutal（填写上下行） |
| 端口轮换 | 关闭 mux 会话 | 仅显式开启；替换底层连接，默认关闭 |
| 端口段 | 支持 | 支持；只有一个端口或未开启时不轮换 |

## 组成

| 目录 | 内容 |
|---|---|
| [`tianma/`](tianma) | 协议核心：QUIC 客户端、SOCKS5、DATAGRAM、OBFS 轮换、门控、策略、限流 |
| [`singbox/`](singbox) | sing-box 适配层：`type: "skyhorse"` 入站/出站 |
| [`docs/`](docs) | 协议、适配、配置、构建、安全、排障文档 |

## 特性

- **传输**：QUIC v1 + TLS 1.3，不使用 HTTP/3
- **TCP**：QUIC 双向流 + 标准 SOCKS5 CONNECT
- **UDP**：QUIC DATAGRAM + 标准 SOCKS5 UDP 头，不进入可靠流
- **认证**：TLS exporter token，通过 SOCKS5 用户名/密码传递，绑定连接
- **混淆**：Salamander（BLAKE2b），多密钥轮换，服务端接受新旧密钥
- **前置门**：HMAC + 时间戳 + nonce，未认证直接丢弃
- **并发**：单连接多流，`max_streams_per_connection` 做流级并发上限
- **拥塞控制**：BBR 可选 `standard` / `conservative` / `aggressive`；留空走 Brutal，按 `up_mbps` / `down_mbps` 发送
- **端口**：端口段多监听；显式开启端口轮换后才换端口，默认关闭，避免误伤长连接
- **治理**：目的地策略、每 IP 连接数、认证失败封禁
- **内核**：连接由适配层 Start/Close 管理，流由 QUIC 原生复用

## 快速开始

```bash
cd singbox
go build -ldflags "-s -w" -o skyhorse ./cmd/skyhorse
./skyhorse check -c server.json
./skyhorse run -c server.json
```

测试：

```bash
cd tianma && go test ./...
cd singbox && go test ./... -count=1
```

## 配置要点

客户端出站关键字段：

```json
{
  "type": "skyhorse",
  "server": "1.2.3.4",
  "server_port": 10000,
  "server_ports": ["10000", "10001", "10002"],
  "max_streams_per_connection": 256,
  "port_hop_enabled": false,
  "port_rotate_seconds": 120,
  "bbr_profile": "standard",
  "up_mbps": 50,
  "down_mbps": 300
}
```

- `port_hop_enabled` 不写或写 `false` 都不轮换；
- 端口数为 1 时不轮换；
- `bbr_profile` 非空走 BBR，`up_mbps` / `down_mbps` 不参与；
- `bbr_profile` 留空走 Brutal，`up_mbps` / `down_mbps` 才生效。

## 文档

- [协议规范](docs/PROTOCOL-QUIC.md)
- [内核与面板适配](docs/ADAPTER.md)
- [配置](docs/CONFIG.md)
- [部署](docs/DEPLOY.md)
- [构建](docs/BUILD.md)
- [安全模型](docs/SECURITY.md)
- [故障排查](docs/TROUBLESHOOTING.md)
- [天马 / 快马面板配置指南](tianma/PANEL-CONFIG-GUIDE.md)

## 状态

| 项 | 状态 |
|---|---|
| 协议核心 | 原生 QUIC，多流，可用 |
| sing-box 内核 | 可用（Windows / Linux / Android） |
| TCP | 已验证 |
| UDP | 已验证 |
| OBFS 轮换 | 已验证 |
| 流并发上限 | 已验证 |
| BBR / Brutal | 可用，需按线路选型 |
| 真实弱网 / DPI | 未充分验证 |

## 已知限制

- 链路仍然是 UDP，无法隐藏 IP 协议号；
- 天马 UDP 每个会话各自占一条 QUIC 连接，会话数受 `max_connections` 限制；
- Brutal 需要填写真实上下行，填错会抢带宽或限速；
- `failure_mode` 字段存在但未差异化执行；
- `obfs_tolerance_seconds` 未做时间窗口校验；
- 无多服务器集群、无 ECH；
- 不与 Hysteria 2 互通。

## 许可

本仓库以 **AGPL-3.0** 发布，见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。

部分拥塞控制实现移植自 Hysteria（MIT）；内核适配层基于 sing-box（GPL-3.0）。
