# SkyHorse（天马）

天马是一个基于 **QUIC + HTTP/3** 的代理协议，以 [Hysteria 2](https://github.com/apernet/hysteria) 为设计参考——**不是它的 fork，也不与它互通**。

SkyHorse 是把天马编进 [sing-box](https://github.com/SagerNet/sing-box) 内核后的产物：一个 `type: "skyhorse"` 的入站/出站。

> 天马不是“更强的 Hy2”，是更可控的 Hy2 分支：认证更强、治理内建、面板客户端一体化；伪装与成熟度不如 Hy2。

## 组成

| 目录 | 内容 |
|---|---|
| [`tianma/`](tianma) | 协议核心：线格式、TLS/QUIC、混淆、拥塞控制、策略、限流、客户端、服务端 |
| [`singbox/`](singbox) | sing-box 适配层：`type: "skyhorse"` 与内核 CLI |
| [`docs/`](docs) | 协议、配置、部署、构建、安全、排障文档 |

## 特性

- **传输**：QUIC + TLS 1.3，ALPN `h3`
- **认证**：TLS exporter 通道绑定 token + HTTP 233，凭据不可跨连接重放
- **TCP**：QUIC 原生双向流 + 帧类型 `0x401` + 1 字节状态
- **UDP**：QUIC Datagram，会话/分片/重组
- **混淆**：Salamander（BLAKE2b + XOR），支持按时间窗口轮换密钥
- **拥塞**：Brutal（带自适应调速）、BBR（带 loss guard）、CC-RX 带宽协商
- **治理**：目的地策略（默认拒绝私网/回环）、按 IP 限流、按用户配额、全局内存水位
- **伪装**：404 / 固定内容 / 本地文件 / 反向代理

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

## 文档

- [协议](docs/PROTOCOL.md)
- [配置](docs/CONFIG.md)
- [部署](docs/DEPLOY.md)
- [构建](docs/BUILD.md)
- [安全模型](docs/SECURITY.md)
- [故障排查](docs/TROUBLESHOOTING.md)

## 状态

| 项 | 状态 |
|---|---|
| 协议核心 | 可用 |
| sing-box 内核 | 可用（Windows / Linux） |
| 面板与客户端 | 独立项目，未包含在本仓库 |
| 真实网络压测 | 长连接建立时十分稳定 |

## 已知限制

- 自签证书下伪装只能“遮住”，不能“像网站”；
- 没有端口跳跃、ECH；
- 不与 Hysteria 2 互通；
- Android 的 `libbox` 需要与 `classes.jar` 同源构建。

## 许可

本仓库以 **AGPL-3.0** 发布，见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。

部分拥塞控制实现移植自 Hysteria（MIT）；内核适配层基于 sing-box（GPL-3.0）。
