# OpenAI API Converter Proxy

一个纯 Go 实现的**双向转换代理**，在 OpenAI **Responses API** 和 **Chat Completions API** 之间进行实时协议转换——支持流式和非流式两种模式。

```
┌──────────────────┐         ┌─────────────────────┐         ┌──────────────────────┐
│  Chat Completions│         │                     │         │   Upstream Responses │
│     Client       │ ──────▶ │   Converter Proxy   │ ──────▶ │        API           │
│                  │ ◀────── │     :9090           │ ◀────── │                      │
└──────────────────┘         │                     │         └──────────────────────┘
                             │                     │
┌──────────────────┐         │                     │         ┌──────────────────────┐
│   Responses API  │         │                     │         │ Upstream Completions │
│     Client       │ ──────▶ │                     │ ──────▶ │        API           │
│                  │ ◀────── │                     │ ◀────── │                      │
└──────────────────┘         └─────────────────────┘         └──────────────────────┘
```

## 特性

- **双向转换**：Chat Completions ↔ Responses API 请求/响应自动互转
- **流式 SSE 转换**：实时将上游 SSE 事件流转换为目标格式
- **Vision（多模态图片）**：`image_url` ↔ `input_image` 自动映射
- **Structured Output**：`response_format` ↔ `text.format` 完整映射（含 JSON Schema）
- **Reasoning（推理控制）**：`reasoning_effort` ↔ `reasoning.effort` 双向转换
- **Tool Calling（函数调用）**：完整的工具调用及流式 tool_calls 转换
- **Refusal（拒绝回复）**：流式和非流式 refusal 内容透传
- **详细 Usage**：`reasoning_tokens`、`cached_tokens` 明细映射
- **零外部依赖**：纯 Go 标准库实现，无任何第三方依赖
- **CORS 支持**：内置跨域中间件，可直接被前端调用
- **API Key 透传**：客户端 `Authorization` 头会转发到上游

## 快速开始

### 前置要求

- Go 1.21+

### 安装与运行

```bash
# 克隆项目
git clone https://gitlab.com/viloze/open-ai-converter.git
cd OPENAI_CONVERTER

# 配置环境变量
cp .env.example .env
# 编辑 .env 填入你的 API 配置

# 编译
go build -o openai-converter .

# 运行
./openai-converter
```

服务器默认在 `http://0.0.0.0:9090` 启动。

### 使用 Docker Compose（推荐）

```bash
# 克隆项目
git clone https://gitlab.com/viloze/open-ai-converter.git
cd OPENAI_CONVERTER

# 配置环境变量
cp .env.example .env
# 编辑 .env 填入你的 API 配置

# 一键启动
docker compose up -d

# 查看日志
docker compose logs -f

# 停止
docker compose down
```

也可以不创建 `.env` 文件，直接通过环境变量启动：

```bash
RESPONSES_API_BASE_URL=https://your-api.com \
RESPONSES_API_KEY=sk-xxx \
docker compose up -d
```

自定义端口：

```bash
PORT=8080 docker compose up -d
# 服务将在 http://localhost:8080 启动
```

### 使用预构建 Docker 镜像（最简单）

无需克隆代码，直接拉取镜像运行：

```bash
docker run -d --name openai-converter \
  -p 9090:9090 \
  -e RESPONSES_API_BASE_URL=https://your-api.com \
  -e RESPONSES_API_KEY=sk-xxx \
  -e COMPLETIONS_API_BASE_URL=https://api.openai.com \
  -e COMPLETIONS_API_KEY=sk-yyy \
  registry.gitlab.com/viloze/open-ai-converter:latest
```

支持 `linux/amd64` 和 `linux/arm64` 架构，Docker 会自动拉取适合你平台的镜像。

## 配置

通过 `.env` 文件或环境变量配置（环境变量优先）：

| 变量名 | 说明 | 默认值 |
|---|---|---|
| `RESPONSES_API_BASE_URL` | 上游 Responses API 地址（方向1的目标） | `https://api.openai.com` |
| `RESPONSES_API_KEY` | 上游 Responses API 密钥 | — |
| `COMPLETIONS_API_BASE_URL` | 上游 Chat Completions API 地址（方向2的目标） | `https://api.openai.com` |
| `COMPLETIONS_API_KEY` | 上游 Chat Completions API 密钥 | — |
| `HOST` | 监听地址 | `0.0.0.0` |
| `PORT` | 监听端口 | `9090` |

也支持命令行参数：

```bash
./openai-converter \
  -responses-url https://your-responses-api.com \
  -responses-key sk-xxx \
  -completions-url https://api.openai.com \
  -completions-key sk-yyy \
  -host 0.0.0.0 \
  -port 8080
```

## API 端点

| 端点 | 方法 | 说明 |
|---|---|---|
| `/v1/chat/completions` | POST | 接收 Chat Completions 请求，转换为 Responses API 调用上游 |
| `/v1/responses` | POST | 接收 Responses 请求，转换为 Chat Completions API 调用上游 |
| `/v1/models` | GET | 透传到上游 Responses API |
| `/v1/*` | * | 其他 `/v1/` 路径透传到上游 |
| `/health` | GET | 健康检查，返回 `{"status":"ok"}` |
| `/` | GET | 服务信息 |

## 使用示例

### 方向1：Chat Completions → Responses API

将标准的 Chat Completions 请求发送到代理，代理自动转换为 Responses API 格式并调用上游。

#### 非流式

```bash
curl http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "Hello!"}
    ],
    "max_tokens": 100
  }'
```

响应格式为标准 Chat Completions 响应：

```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "created": 1700000000,
  "model": "gpt-4.1-nano",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Hello! How can I help you?"
    },
    "finish_reason": "stop"
  }],
  "usage": {
    "prompt_tokens": 20,
    "completion_tokens": 8,
    "total_tokens": 28
  }
}
```

#### 流式

```bash
curl -N http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

返回标准 SSE 格式的 `chat.completion.chunk`：

```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{...}}

data: [DONE]
```

### 方向2：Responses API → Chat Completions

将 Responses API 格式的请求发送到代理，代理自动转换为 Chat Completions 格式调用上游。

#### 非流式

```bash
curl http://localhost:9090/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-key" \
  -d '{
    "model": "gpt-4.1-nano",
    "input": [
      {"role": "user", "content": "Hello!"}
    ],
    "max_output_tokens": 100
  }'
```

响应格式为标准 Responses API 响应：

```json
{
  "id": "resp_xxx",
  "object": "response",
  "created_at": 1700000000,
  "status": "completed",
  "model": "gpt-4.1-nano",
  "output": [{
    "id": "msg_xxx",
    "type": "message",
    "status": "completed",
    "role": "assistant",
    "content": [{
      "type": "output_text",
      "text": "Hello! How can I help you?",
      "annotations": []
    }]
  }],
  "usage": {
    "input_tokens": 20,
    "output_tokens": 8,
    "total_tokens": 28
  }
}
```

#### 流式

```bash
curl -N http://localhost:9090/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "input": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

返回 Responses API 标准 SSE 事件流：

```
event: response.created
data: {"type":"response.created","response":{...},"sequence_number":0}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{...},"sequence_number":2}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello","sequence_number":4}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"!","sequence_number":5}

event: response.output_text.done
data: {"type":"response.output_text.done","text":"Hello!","sequence_number":8}

event: response.completed
data: {"type":"response.completed","response":{...},"sequence_number":10}
```

### Vision（多模态图片）

#### Chat Completions 侧（image_url）

```bash
curl http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "messages": [{
      "role": "user",
      "content": [
        {"type": "text", "text": "What is in this image?"},
        {"type": "image_url", "image_url": {"url": "https://example.com/photo.jpg", "detail": "high"}}
      ]
    }]
  }'
```

代理自动将 `image_url` 转换为 Responses API 的 `input_image` 格式。

#### Responses 侧（input_image）

```bash
curl http://localhost:9090/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "input": [{
      "role": "user",
      "content": [
        {"type": "input_text", "text": "What is in this image?"},
        {"type": "input_image", "image_url": "https://example.com/photo.jpg", "detail": "high"}
      ]
    }]
  }'
```

代理自动将 `input_image` 转换为 Chat Completions 的 `image_url` 格式。

### Structured Output（结构化输出）

#### Chat Completions 侧（response_format）

```bash
curl http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "messages": [{"role": "user", "content": "Extract name and age from: John is 30 years old"}],
    "response_format": {
      "type": "json_schema",
      "json_schema": {
        "name": "person",
        "strict": true,
        "schema": {
          "type": "object",
          "properties": {
            "name": {"type": "string"},
            "age": {"type": "integer"}
          },
          "required": ["name", "age"]
        }
      }
    }
  }'
```

代理将 `response_format` 转换为 Responses API 的 `text.format` 格式。

#### Responses 侧（text.format）

```bash
curl http://localhost:9090/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "input": [{"role": "user", "content": "Extract name and age from: John is 30 years old"}],
    "text": {
      "format": {
        "type": "json_schema",
        "name": "person",
        "strict": true,
        "schema": {
          "type": "object",
          "properties": {
            "name": {"type": "string"},
            "age": {"type": "integer"}
          },
          "required": ["name", "age"]
        }
      }
    }
  }'
```

### Tool Calling（函数调用）

#### Chat Completions 侧

```bash
curl http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "messages": [{"role": "user", "content": "What is the weather in Tokyo?"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Get current weather",
        "parameters": {
          "type": "object",
          "properties": {
            "location": {"type": "string"}
          },
          "required": ["location"]
        }
      }
    }],
    "tool_choice": "auto"
  }'
```

代理将 `tools` 转换为 Responses API 的函数工具格式，并在流式模式下正确拼接 `function_call_arguments.delta` 事件为 `tool_calls` chunk。

#### 多轮对话（tool result）

```bash
curl http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "messages": [
      {"role": "user", "content": "What is the weather in Tokyo?"},
      {"role": "assistant", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"location\":\"Tokyo\"}"}}]},
      {"role": "tool", "tool_call_id": "call_1", "content": "{\"temp\": 22, \"condition\": \"sunny\"}"}
    ]
  }'
```

代理将 `tool` 消息转换为 Responses API 的 `function_call_output`：

```
assistant tool_calls → function_call (type: function_call)
tool message         → function_call_output (type: function_call_output)
```

### 多轮对话

#### Chat Completions 侧

```bash
curl http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "messages": [
      {"role": "user", "content": "My name is Alice."},
      {"role": "assistant", "content": "Hello Alice! How can I help you?"},
      {"role": "user", "content": "What is my name?"}
    ]
  }'
```

代理自动将历史 assistant 消息转换为 Responses API 的 `type: message` 格式（包含 `output_text` 内容数组），确保多轮对话上下文正确传递。

#### Responses 侧

```bash
curl http://localhost:9090/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1-nano",
    "input": [
      {"role": "user", "content": "My name is Alice."},
      {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Hello Alice! How can I help you?"}]},
      {"role": "user", "content": "What is my name?"}
    ]
  }'
```

代理自动将 Responses API 的 assistant 消息（含 `output_text` 数组）转换为 Chat Completions 的 `{"role": "assistant", "content": "..."}` 格式。

### Reasoning（推理控制）

```bash
# Chat Completions 侧：reasoning_effort
curl http://localhost:9090/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "o3-mini",
    "messages": [{"role": "user", "content": "Solve this step by step: 2+2*2"}],
    "reasoning_effort": "high"
  }'
# → 转换为 Responses API: {"reasoning": {"effort": "high"}}

# Responses 侧：reasoning.effort
curl http://localhost:9090/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "o3-mini",
    "input": [{"role": "user", "content": "Solve: 2+2*2"}],
    "reasoning": {"effort": "high"}
  }'
# → 转换为 Chat Completions: {"reasoning_effort": "high"}
```

## 参数映射表

### 请求参数

| Chat Completions | Responses API | 说明 |
|---|---|---|
| `model` | `model` | 直接映射 |
| `messages` | `input` | 消息格式互转 |
| `messages[role=system]` | `instructions` | 系统提示词 |
| `messages[role=developer]` | `instructions` | 开发者消息 → 指令 |
| `stream` | `stream` | 直接映射 |
| `max_tokens` | `max_output_tokens` | 名称映射 |
| `max_completion_tokens` | `max_output_tokens` | 优先使用（更新的字段） |
| `temperature` | `temperature` | 直接映射 |
| `top_p` | `top_p` | 直接映射 |
| `frequency_penalty` | `frequency_penalty` | 直接映射 |
| `presence_penalty` | `presence_penalty` | 直接映射 |
| `tools` | `tools` | 工具格式互转 |
| `tool_choice` | `tool_choice` | 直接透传 |
| `parallel_tool_calls` | `parallel_tool_calls` | 直接透传 |
| `stop` | `stop` | 透传（部分实现支持） |
| `seed` | `seed` | 透传（部分实现支持） |
| `n` | — | ⚠️ Responses API 不支持 |
| `user` | `user` | 直接映射 |
| `response_format` | `text.format` | 结构化输出互转 |
| `logprobs` + `top_logprobs` | `top_logprobs` | 合并映射 |
| `reasoning_effort` | `reasoning.effort` | 推理控制互转 |
| `service_tier` | `service_tier` | 直接透传 |
| `store` | `store` | 直接透传 |
| `metadata` | `metadata` | 直接透传 |
| `stream_options` | — | 方向2自动注入 `include_usage: true` |

### 响应参数

| Chat Completions | Responses API | 说明 |
|---|---|---|
| `id` (chatcmpl-) | `id` (resp_) | 前缀自动转换 |
| `choices[0].message.content` | `output[0].content[0].text` | 文本内容映射 |
| `choices[0].message.tool_calls` | `output[].type=function_call` | 工具调用映射 |
| `choices[0].message.refusal` | `output[0].content[0].type=refusal` | 拒绝内容映射 |
| `choices[0].finish_reason=stop` | `status=completed` | 完成状态 |
| `choices[0].finish_reason=length` | `status=incomplete` | 截断状态 |
| `choices[0].finish_reason=tool_calls` | 含 `function_call` 输出 | 工具调用状态 |
| `usage.prompt_tokens` | `usage.input_tokens` | 名称映射 |
| `usage.completion_tokens` | `usage.output_tokens` | 名称映射 |
| `usage.completion_tokens_details.reasoning_tokens` | `usage.output_tokens_details.reasoning_tokens` | 推理 token 明细 |
| `usage.prompt_tokens_details.cached_tokens` | `usage.input_tokens_details.cached_tokens` | 缓存 token 明细 |
| `service_tier` | `service_tier` | 直接透传 |

### 流式事件映射

#### 方向1：Responses SSE → Chat Completions SSE

| Responses 事件 | Chat Completions chunk |
|---|---|
| `response.output_text.delta` | `delta.content` |
| `response.refusal.delta` | `delta.refusal` |
| `response.function_call_arguments.delta` | `delta.tool_calls[].function.arguments` |
| `response.output_item.added` (function_call) | tool_call 初始化（id, name） |
| `response.completed` | `finish_reason` + `usage` + `[DONE]` |

#### 方向2：Chat Completions SSE → Responses SSE

| Chat Completions chunk | Responses 事件 |
|---|---|
| 首个 chunk | `response.created` + `response.in_progress` + `response.output_item.added` + `response.content_part.added` |
| `delta.content` | `response.output_text.delta` |
| `delta.refusal` | `response.refusal.delta` |
| `delta.tool_calls` (初始) | `response.output_item.added` (function_call) |
| `delta.tool_calls` (后续) | `response.function_call_arguments.delta` |
| `finish_reason` / `[DONE]` | `response.output_text.done` + `response.content_part.done` + `response.output_item.done` + `response.completed` |

## 消息格式转换

### Chat Completions → Responses API

```
system / developer  →  instructions 字段
user                →  { role: "user", content: ... }
assistant           →  { role: "assistant", content: ... }
assistant (w/ tool_calls)  →  content (if any) + function_call items
tool                →  { type: "function_call_output", call_id: ..., output: ... }
```

### Responses API → Chat Completions

```
instructions        →  { role: "system", content: ... }
{ role: "user" }    →  { role: "user", content: ... }
{ role: "assistant" } →  { role: "assistant", content: ... }
{ type: "function_call" }  →  { role: "assistant", tool_calls: [...] }
{ type: "function_call_output" }  →  { role: "tool", tool_call_id: ..., content: ... }
```

## 不支持的功能

以下功能由于两个 API 之间架构差异过大，暂无法转换：

| 功能 | 原因 |
|---|---|
| `n > 1`（多选项） | Responses API 不支持一次生成多个候选 |
| `web_search` 工具 | Chat Completions 无对应工具类型（静默跳过） |
| `file_search` 工具 | Chat Completions 无对应工具类型（静默跳过） |
| `code_interpreter` 工具 | Chat Completions 无对应工具类型（静默跳过） |
| `computer_use` 工具 | Chat Completions 无对应工具类型（静默跳过） |
| `previous_response_id` | Chat Completions 无会话链概念 |
| `truncation` | Chat Completions 无自动截断策略 |
| `logprobs` 详细值 | 两个 API 的 logprobs 结构不同，仅映射数量 |
| `reasoning.summary` | Chat Completions 无 reasoning summary 支持 |
| `modalities` (audio) | 两个 API 的音频处理方式不同 |
| `prediction` | Chat Completions 独有功能 |

## 项目结构

```
OPENAI_CONVERTER/
├── .env              # 环境变量配置
├── .env.example      # 环境变量示例
├── .gitlab-ci.yml    # GitLab CI/CD（多平台构建 & 容器发布）
├── go.mod            # Go 模块定义（零外部依赖）
├── main.go           # 入口、路由、中间件（CORS/日志）、.env 加载
├── types.go          # 全部类型定义（Chat Completions & Responses API）
├── convert.go        # 核心双向转换逻辑（请求/响应/Vision/Schema/Reasoning）
├── handler.go        # HTTP 处理器 & 流式 SSE 转换
├── Dockerfile        # 多阶段构建（golang:1.23-alpine → alpine:3.19）
├── docker-compose.yml # Docker Compose 编排
└── README.md         # 本文件
```

### 源码说明

| 文件 | 行数 | 职责 |
|---|---|---|
| `main.go` | ~170 | HTTP 服务器启动、路由注册、CORS/日志中间件、`.env` 加载 |
| `types.go` | ~350 | 两个 API 的完整类型定义、流式事件类型、辅助函数 |
| `convert.go` | ~780 | 请求/响应双向转换、Vision 图片转换、Structured Output 映射、Reasoning 映射 |
| `handler.go` | ~780 | HTTP handler、非流式处理、双向流式 SSE 事件转换、上游请求 |

## API Key 处理

请求中的 `Authorization: Bearer <key>` 头会被提取并用于上游请求：

- **方向1** (`/v1/chat/completions`)：使用客户端提供的 key 或 fallback 到 `RESPONSES_API_KEY`
- **方向2** (`/v1/responses`)：使用客户端提供的 key 或 fallback 到 `COMPLETIONS_API_KEY`

这使得代理可以：
1. 作为透明代理，让客户端使用自己的 key
2. 作为网关，使用统一的 key 对外提供服务

## 错误处理

- 上游返回非 200 状态码时，错误响应直接透传到客户端
- 请求解析失败返回 400
- 转换错误返回 500
- 上游连接失败返回 502
- 所有错误使用 OpenAI 标准错误格式：

```json
{
  "error": {
    "message": "error description",
    "type": "proxy_error",
    "code": 502
  }
}
```

## Changelog

### v1.1.0

- **fix**: 修复多轮对话在第二轮请求时失败的问题（assistant 消息格式修正为 `type: message` + `output_text` 数组）
- **fix**: 修复 `contentToString` 对 `null` 内容的处理
- **build**: Docker Compose 端口支持通过 `PORT` 环境变量动态配置
- **ci**: 新增 Container Registry 推送（`registry.gitlab.com/viloze/open-ai-converter`）

### v1.0.0

- 初始版本
- 双向转换 Chat Completions ↔ Responses API（流式 & 非流式）
- Vision、Structured Output、Reasoning、Tool Calling、Refusal、Logprobs 完整支持
- GitLab CI/CD 多平台构建（linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64）
- Docker 多架构镜像发布

## License

MIT
