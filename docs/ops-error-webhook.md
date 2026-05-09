# Ops 错误 Webhook 推送

每当系统记录一条请求错误（请求失败、上游错误、鉴权失败等）时，可以通过 Webhook 将原始错误数据实时推送到你自己的服务端。

---

## 配置

在 `config.yaml` 中添加以下配置：

```yaml
ops:
  webhook:
    url: "https://your-server.com/ops/errors"   # 接收端点，留空则关闭
    secret: "your-hmac-secret"                   # 可选，用于签名验证
    timeout_seconds: 5                           # 单次请求超时（秒），默认 5
    max_retries: 2                               # 失败重试次数，默认 2
    buffer_size: 2048                            # 内部缓冲队列大小，默认 2048
```

- `url` 为空 = 功能关闭，零运行开销
- `secret` 为空 = 不签名，不发送签名 Header

---

## 推送时机

每条错误写入数据库**成功后**立即投递，异步发送，不阻塞请求主路径。

- 推送频率：每条错误独立发一次 POST
- 失败重试：最多重试 `max_retries` 次（指数退避，基础延迟 300ms）
- 缓冲溢出：若积压超过 `buffer_size`，新事件被静默丢弃并打印一条告警日志（每 10 秒最多一次）

---

## 请求格式

### HTTP 请求

```
POST {url}
Content-Type: application/json
X-Sub2Api-Timestamp: 1746789781          # 配置了 secret 时才有
X-Sub2Api-Signature: sha256=<hex>        # 配置了 secret 时才有
```

### 请求体

```json
{
  "event": "error",
  "timestamp": "2026-05-09T14:23:01Z",
  "error": {
    "phase": "upstream",
    "type": "upstream_error",
    "severity": "P1",
    "status_code": 502,

    "platform": "openai",
    "model": "gpt-4o",
    "message": "Bad Gateway",
    "request_path": "/v1/chat/completions",

    "user_id": 88,
    "group_id": 3,
    "account_id": 12,

    "request_id": "req_abc123",
    "client_request_id": "cli_xyz456",

    "upstream_status_code": 502,
    "upstream_error_message": "Bad Gateway",

    "is_retryable": true,
    "retry_count": 1,

    "upstream_latency_ms": 1234,
    "response_latency_ms": 1300,

    "created_at": "2026-05-09T14:23:01Z"
  }
}
```

---

## 字段说明

### 顶层字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `event` | string | 固定值 `"error"` |
| `timestamp` | string | 推送时间（RFC3339 UTC） |
| `error` | object | 错误详情，见下表 |

### `error` 对象字段

#### 分类字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `phase` | string | 错误发生阶段，见[阶段枚举](#phase-阶段枚举) |
| `type` | string | 错误类型，见[类型枚举](#type-错误类型枚举) |
| `severity` | string | 严重性：`P1`（5xx/429）/ `P2`（其他4xx）/ `P3`（客户端/计费类） |
| `status_code` | int | 返回给客户端的 HTTP 状态码 |

#### 上下文字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `platform` | string | 上游平台，如 `openai`、`anthropic`、`gemini` |
| `model` | string | 请求的模型名称 |
| `message` | string | 错误摘要信息 |
| `request_path` | string | 客户端请求路径，如 `/v1/chat/completions` |
| `user_id` | int \| null | 发起请求的用户 ID |
| `group_id` | int \| null | 所属渠道组 ID |
| `account_id` | int \| null | 实际使用的上游账号 ID |
| `request_id` | string | 系统内部请求 ID |
| `client_request_id` | string | 客户端传入的请求 ID（`X-Request-Id` Header） |

#### 上游错误字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `upstream_status_code` | int \| null | 上游返回的 HTTP 状态码 |
| `upstream_error_message` | string \| null | 上游返回的原始错误信息 |

#### 重试与延迟字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `is_retryable` | bool | 该错误是否可重试 |
| `retry_count` | int | 已重试次数 |
| `upstream_latency_ms` | int \| null | 上游响应耗时（毫秒） |
| `response_latency_ms` | int \| null | 响应客户端总耗时（毫秒） |
| `created_at` | string | 错误发生时间（RFC3339 UTC） |

---

## `phase` 阶段枚举

| 值 | 含义 | 责任方 |
|---|---|---|
| `upstream` | 上游服务返回错误（5xx、429、overloaded） | 上游厂商 |
| `network` | 网络层异常（连接超时、TLS 错误） | 上游厂商 |
| `request` | 客户端请求问题（余额不足、订阅无效、并发超限） | 客户端 |
| `auth` | 鉴权失败（API Key 无效或缺失） | 客户端 |
| `routing` | 路由失败（无可用上游账号） | 平台 |
| `internal` | 平台内部错误 | 平台 |

---

## `type` 错误类型枚举

| 值 | 典型场景 |
|---|---|
| `upstream_error` | 上游返回 5xx |
| `overloaded_error` | 上游过载（HTTP 529） |
| `rate_limit_error` | 上游 429 限速 |
| `authentication_error` | API Key 失效 |
| `invalid_request_error` | 请求参数错误 |
| `billing_error` | 上游账号余额耗尽 |
| `subscription_error` | 订阅无效或未找到 |
| `api_error` | 兜底错误类型 |

---

## 签名验证

配置了 `secret` 时，每个请求会携带两个额外 Header：

| Header | 说明 |
|---|---|
| `X-Sub2Api-Timestamp` | Unix 时间戳（秒级字符串） |
| `X-Sub2Api-Signature` | `sha256=<hex(HMAC-SHA256(secret, timestamp + "." + body))>` |

### 签名算法

```
签名原文 = timestamp + "." + 请求体原始字节
签名值   = sha256= + hex( HMAC-SHA256(secret, 签名原文) )
```

### 验签示例

**Python**

```python
import hmac
import hashlib

def verify_webhook(secret: str, request_headers: dict, body: bytes) -> bool:
    ts = request_headers.get("X-Sub2Api-Timestamp", "")
    sig = request_headers.get("X-Sub2Api-Signature", "")
    if not ts or not sig:
        return False
    expected = "sha256=" + hmac.new(
        secret.encode(),
        f"{ts}.".encode() + body,
        hashlib.sha256,
    ).hexdigest()
    return hmac.compare_digest(sig, expected)
```

**Go**

```go
import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "fmt"
    "net/http"
)

func VerifyWebhook(secret string, r *http.Request, body []byte) bool {
    ts := r.Header.Get("X-Sub2Api-Timestamp")
    sig := r.Header.Get("X-Sub2Api-Signature")
    if ts == "" || sig == "" {
        return false
    }
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(ts))
    mac.Write([]byte("."))
    mac.Write(body)
    expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(sig), []byte(expected))
}
```

**Node.js**

```javascript
const crypto = require("crypto");

function verifyWebhook(secret, headers, body) {
  const ts = headers["x-sub2api-timestamp"];
  const sig = headers["x-sub2api-signature"];
  if (!ts || !sig) return false;
  const expected =
    "sha256=" +
    crypto
      .createHmac("sha256", secret)
      .update(`${ts}.`)
      .update(body)
      .digest("hex");
  return crypto.timingSafeEqual(Buffer.from(sig), Buffer.from(expected));
}
```

> **建议**：使用常量时间比较（`hmac.compare_digest` / `crypto.timingSafeEqual`），防止时序攻击。

---

## 接收端建议

1. **立即返回 2xx**：Webhook 超时后会重试，接收端应尽快响应，异步处理数据
2. **幂等处理**：重试可能导致同一条错误被发送多次，可用 `request_id` 做幂等去重
3. **忽略非 `error` 事件**：`event` 字段将来可能扩展，建议只处理 `"error"` 类型

---

## 常见过滤场景

接收方可根据以下字段过滤感兴趣的错误：

```python
# 只处理上游错误
if payload["error"]["phase"] in ("upstream", "network"):
    handle_upstream_error(payload)

# 只处理严重错误
if payload["error"]["severity"] == "P1":
    send_alert(payload)

# 只处理特定平台
if payload["error"]["platform"] == "openai":
    handle_openai_error(payload)
```
