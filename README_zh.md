# commandcode2api（中文文档）

> Command Code API → OpenAI / Anthropic 兼容端点，Go 实现。
> 单一静态二进制、CGO-free、可选服务端多 Key 池。

[MAXeaglet/commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy) 的 Go
重写版。基于官方 CLI 网络流量抓包分析，精确复刻 Command Code API 请求协议 ——
设备指纹、生命周期预请求、以及 `command-code@1.53.1` 的完整 wire 方言。

**特性**：OpenAI Chat Completions / **Responses API (`/v1/responses`)** + Anthropic
Messages API | 流式 & 非流式 | 工具调用 (tool_use) | 多模态图片输入 | reasoning
effort | 动态模型列表 | 缓存命中指标 | 设备指纹伪装（每 Key 确定性、自动刷新）|
`x-api-key` 认证 | 客户端断连时打断上游 | 零输出 → 429 自动重试 | 连续超时提示 |
隐私日志 | **服务端多 Key 池**（轮换、会话粘滞、配额追踪、Admin REST）。

**为什么用 Go**：~15MB 静态二进制（无需 Node 运行时），稳态 RSS 个位数 MB，
请求体内存峰值约为参考实现的 1/2（Node 实测 5.5× 放大），背压由 TCP 原生处理。

## 快速开始

```bash
go build -o commandcode2api .
./commandcode2api          # 读取 ./config.json，监听 :3050（仓库自带配置）
```

API Key 通过 `Authorization`（Anthropic SDK 用 `x-api-key`）按请求传入，必须以
`user_` 开头：

```bash
curl http://127.0.0.1:3050/v1/chat/completions \
  -H "Authorization: Bearer user_xxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

Docker：`docker compose up -d`

## Key 池（可选）

零配置时行为与原版完全一致（客户端自带 `user_` Key 直通）。要启用池化网关
（客户端只持有一个 `sk-` Key，代理轮换真实 `user_` Key）：

```bash
./commandcode2api -state-dir ./data   # 配 CC_VAULT_SECRET / CC_ADMIN_TOKEN 环境变量
```

- `POST /admin/keys`：添加池内 Key（`{"name","key","priority"}`）
- `POST /admin/gateway-keys`：签发网关 Key（明文只显示一次）
- 选择策略：优先级分层轮询 + 1h 会话粘滞（保护上游 prompt cache）；
  429/402/USAGE_EXCEEDED 触发冷却并自动故障转移到下一个 Key
- `CC_POOL_ONLY=1` 关闭直通；`CC_ADMIN_TOKEN` 保护 `/admin/*`

## 配置

与 commandcode-proxy 完全兼容：相同的 `config.json` 字段、相同的环境变量、
相同的默认值。完整表见 [README.md](README.md#configuration)。

核心环境变量：`PORT`/`HOST`、`CC_API_BASE`、`CC_UPSTREAM_PROXY`（上游 CONNECT
代理，指纹/生命周期请求同走 —— 一个账号一个 IP）、`CMD_ZDR`、
`CC_FINGERPRINT_SALT`（整批换设备）、`CC_EMPTY_SYSTEM_PLACEHOLDER`（issue #17）、
`CC_STREAM_IDLE_MS`/`CC_NONSTREAM_IDLE_MS`、`CC_MAX_INFLIGHT`、
`CC_KEEPALIVE_TIMEOUT_MS`（反代 keepalive 必须小于它）。

## 错误语义（1:1 移植）

上游 402 → 429（支付失败按限流处理）；流内 error 事件尊重上游 `statusCode`
（消息内 `<NNN>` 前缀优先）；截断绝不谎报完成：`max_output_tokens` 等一族 →
`length`/`max_tokens`/`incomplete`，`pause_turn` 在 Anthropic 原样透出；
无 finish 事件 → 可重试 `502 upstream_error`，绝不补发 `[DONE]` / `message_stop`；
零输出 → 429（防虚假计费）；Anthropic `input_tokens` 只计非缓存部分（issue #25）。

## 免责声明

仅供学习研究。非官方项目，与 Command Code 无关。使用需自行承担责任并遵守
Command Code 服务条款。API Key 按请求转发，绝不入日志；池内 Key 落盘加密。

## 许可

MIT，见 [LICENSE](LICENSE)。移植自
[MAXeaglet/commandcode-proxy](https://github.com/MAXeaglet/commandcode-proxy)。
