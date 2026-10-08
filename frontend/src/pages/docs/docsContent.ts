import {
  resolveTemplate,
  type DocsLocale,
  type QuickTool,
} from "./quickStartTools";
import { addEndpointDetails, buildExtraEndpoints } from "./endpointDetails";
import { buildGuides, guideToMarkdown } from "./docsGuides";

export type ParameterSpec = {
  name: string;
  type: string;
  required: boolean;
  description: string;
  defaultValue?: string;
};
export type ResponseExample = {
  code: number;
  body: string;
  label?: string;
  lang?: string;
};

export type EndpointSpec = {
  id: string;
  method: string;
  path: string;
  title: string;
  description: string;
  curl: string;
  defaultBody?: string;
  responses: ResponseExample[];
  category?: "text" | "media" | "advanced";
  transport?: "websocket";
  parameters?: ParameterSpec[];
  requestExamples?: { label: string; lang: string; content: string }[];
};

function copy(locale: DocsLocale, zh: string, en: string) {
  return locale === "zh" ? zh : en;
}

export function buildEndpointSpecs(
  baseUrl: string,
  locale: DocsLocale = "zh",
): EndpointSpec[] {
  const endpoints: EndpointSpec[] = [
    {
      id: "api-responses",
      method: "POST",
      path: "/v1/responses",
      title: copy(locale, "创建 Responses 响应", "Create Responses output"),
      description: copy(
        locale,
        "Codex Responses API 原生端点，支持流式响应，直接转发到上游服务。",
        "Native Codex Responses endpoint with optional streaming, forwarded directly upstream.",
      ),
      defaultBody: `{
  "model": "gpt-5.5",
  "input": [{"role": "user", "content": [{"type": "input_text", "text": "Hello"}]}],
  "stream": false
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/v1/responses \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-5.5",
  "input": [
    {"role": "user", "content": [{"type": "input_text", "text": "Hello, what can you do?"}]}
  ],
  "stream": false,
  "reasoning": {"effort": "high"}
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "id": "resp_abc123",
  "object": "response",
  "model": "gpt-5.5",
  "status": "completed",
  "output": [
    {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Hello!"}]}
  ],
  "usage": {"input_tokens": 12, "output_tokens": 45, "total_tokens": 57}
}`,
        },
        {
          code: 400,
          body: `{
  "error": {"code": "invalid_request", "message": "model is required", "type": "invalid_request_error"}
}`,
        },
        {
          code: 401,
          body: `{
  "error": {"code": "invalid_api_key", "message": "Invalid API key provided", "type": "authentication_error"}
}`,
        },
        {
          code: 503,
          body: `{
  "error": {"message": "无可用账号，请稍后重试", "type": "server_error", "code": "no_available_account"}
}`,
        },
        {
          code: 503,
          label: copy(locale, "额度耗尽", "Quota exhausted"),
          body: `{
  "error": {"message": "Rate limit exceeded", "type": "server_error", "code": "account_pool_usage_limit_reached", "resets_in_seconds": 18000}
}`,
        },
      ],
    },
    {
      id: "api-chat",
      method: "POST",
      path: "/v1/chat/completions",
      title: copy(
        locale,
        "创建 Chat Completions 响应",
        "Create Chat Completions output",
      ),
      description: copy(
        locale,
        "OpenAI Chat Completions 兼容端点，会在 OpenAI 与 Codex Responses 格式之间自动转换。",
        "OpenAI Chat Completions compatible endpoint that translates between OpenAI and Codex Responses formats.",
      ),
      defaultBody: `{
  "model": "gpt-5.5",
  "messages": [{"role": "user", "content": "Hello"}],
  "stream": false
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/v1/chat/completions \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-5.5",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "Hello!"}
  ],
  "stream": false,
  "reasoning_effort": "high"
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "model": "gpt-5.5",
  "choices": [
    {"index": 0, "message": {"role": "assistant", "content": "Hello! How can I help you today?"}, "finish_reason": "stop"}
  ],
  "usage": {"prompt_tokens": 18, "completion_tokens": 9, "total_tokens": 27}
}`,
        },
        {
          code: 400,
          body: `{
  "error": {"code": "invalid_request", "message": "Request validation failed", "type": "invalid_request_error"}
}`,
        },
        {
          code: 401,
          body: `{
  "error": {"code": "invalid_api_key", "message": "Invalid API key provided", "type": "authentication_error"}
}`,
        },
      ],
    },
    {
      id: "api-messages",
      method: "POST",
      path: "/v1/messages",
      title: copy(locale, "创建 Messages 响应", "Create Messages output"),
      description: copy(
        locale,
        "Anthropic Messages 兼容端点。支持 Claude OAuth、Setup Token 和 API Key + Base URL 原生路径；Codex 转换回退受密钥渠道、账号与路由设置约束，模型按系统设置映射。",
        "Anthropic Messages compatible endpoint with native Claude OAuth, Setup Token and API Key + Base URL paths. Codex translation fallback depends on key channel, account and routing settings, with configured model mappings.",
      ),
      defaultBody: `{
  "model": "claude-sonnet-4-5",
  "max_tokens": 1024,
  "messages": [{"role": "user", "content": "Hello"}]
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/v1/messages \\
  --header 'x-api-key: <token>' \\
  --header 'Content-Type: application/json' \\
  --header 'anthropic-version: 2023-06-01' \\
  --data '{
  "model": "claude-sonnet-4-5",
  "max_tokens": 1024,
  "messages": [{"role": "user", "content": "Hello, Claude!"}]
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "id": "msg_abc123",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5",
  "content": [{"type": "text", "text": "Hello! How can I assist you today?"}],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {"input_tokens": 10, "output_tokens": 12, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
}`,
        },
        {
          code: 400,
          body: `{
  "type": "error",
  "error": {"type": "invalid_request_error", "message": "model is required"}
}`,
        },
        {
          code: 401,
          body: `{
  "type": "error",
  "error": {"type": "authentication_error", "message": "Invalid API key"}
}`,
        },
        {
          code: 429,
          body: `{
  "type": "error",
  "error": {"type": "rate_limit_error", "message": "All accounts rate limited"}
}`,
        },
      ],
    },
    {
      id: "api-images-gen",
      method: "POST",
      path: "/v1/images/generations",
      title: copy(locale, "生成图片", "Generate images"),
      description: copy(
        locale,
        "支持 GPT Image 2 / 2.5 及可用 Grok Imagine 模型，按模型选择上游。GPT Image 路径的 response_format 默认 b64_json；配置 S3 兼容存储后，url 返回 1 小时预签名链接，否则返回 base64 data URL。",
        "Supports GPT Image 2 / 2.5 and available Grok Imagine models, routed by model name. On the GPT Image path, response_format defaults to b64_json; with S3-compatible storage, url returns a 1-hour signed link, otherwise a base64 data URL.",
      ),
      defaultBody: `{
  "model": "gpt-image-2",
  "prompt": "Draw a small orange cat",
  "size": "1024x1024",
  "quality": "high"
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/v1/images/generations \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-image-2",
  "prompt": "Draw a small orange cat",
  "response_format": "b64_json"
}'`,
      responses: [
        {
          code: 200,
          label: "Base64",
          body: `{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [{"b64_json": "..."}],
  "usage": {"images": 1}
}`,
        },
        {
          code: 200,
          label: "URL",
          body: `{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [{"url": "https://<bucket-endpoint>/images/api/...png?X-Amz-Expires=3600&X-Amz-Signature=..."}],
  "usage": {"images": 1}
}`,
        },
      ],
    },
    {
      id: "api-images-edit",
      method: "POST",
      path: "/v1/images/edits",
      title: copy(locale, "编辑图片", "Edit images"),
      description: copy(
        locale,
        "OpenAI Images 编辑兼容端点，支持 JSON image_url 和 multipart 文件上传。",
        "OpenAI Images edit compatible endpoint supporting JSON image_url input and multipart uploads.",
      ),
      defaultBody: `{
  "model": "gpt-image-2",
  "prompt": "Replace the background with aurora lights",
  "images": [{"image_url": "https://example.com/source.png"}],
  "output_format": "png"
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/v1/images/edits \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-image-2",
  "prompt": "Replace the background with aurora lights",
  "images": [{"image_url": "https://example.com/source.png"}]
}'`,
      requestExamples: [
        {
          label: "multipart",
          lang: "bash",
          content: `curl '${baseUrl}/v1/images/edits' \\
  -H 'Authorization: Bearer YOUR_API_KEY' \\
  -F 'model=gpt-image-2.5-flare' \\
  -F 'prompt=Replace the background with aurora lights' \\
  -F 'image=@source.png' \\
  -F 'mask=@mask.png'`,
        },
      ],
      responses: [
        {
          code: 200,
          body: `{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [{"b64_json": "..."}]
}`,
        },
      ],
    },
    {
      id: "api-models",
      method: "GET",
      path: "/v1/models",
      title: copy(locale, "列出模型", "List models"),
      description: copy(
        locale,
        "列出当前代理对外暴露的可用模型。",
        "List the models currently exposed by this proxy.",
      ),
      curl: `curl --request GET \\
  --url ${baseUrl}/v1/models \\
  --header 'Authorization: Bearer <token>'`,
      responses: [
        {
          code: 200,
          body: `{
  "object": "list",
  "data": [
    {"id": "gpt-6-astra", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.6-sol", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.6-terra", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.6-luna", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.3-codex-spark", "object": "model", "owned_by": "openai"},
    {"id": "gpt-image-2", "object": "model", "owned_by": "openai"}
  ]
}`,
        },
        {
          code: 401,
          body: `{
  "error": {"code": "invalid_api_key", "message": "Invalid API key provided", "type": "authentication_error"}
}`,
        },
      ],
    },
    {
      id: "api-health",
      method: "GET",
      path: "/health",
      title: copy(locale, "健康检查", "Health check"),
      description: copy(
        locale,
        "查看服务状态和可用账号数量；该端点不需要认证。",
        "Inspect service status and available account count; this endpoint does not require authentication.",
      ),
      curl: `curl --request GET \\
  --url ${baseUrl}/health`,
      responses: [
        {
          code: 200,
          body: `{
  "status": "ok",
  "available": 5,
  "total": 8
}`,
        },
      ],
    },
  ];
  return [...endpoints, ...buildExtraEndpoints(baseUrl, locale)].map(
    (endpoint) => addEndpointDetails(endpoint, locale),
  );
}

export function buildAdminSpecs(
  baseUrl: string,
  locale: DocsLocale = "zh",
): EndpointSpec[] {
  return [
    {
      id: "admin-add-rt",
      method: "POST",
      path: "/api/admin/accounts",
      title: copy(
        locale,
        "添加账号（Refresh Token）",
        "Add account (Refresh Token)",
      ),
      description: copy(
        locale,
        "通过 Refresh Token 添加账号，系统会自动刷新 Access Token 并加入号池。可在 custom_headers 中指定 Chatgpt-Account-Id，把同一登录身份的不同工作区作为独立路由保存。",
        "Add an account via Refresh Token. The system refreshes its Access Token and adds it to the pool automatically. Set Chatgpt-Account-Id in custom_headers to keep separate workspace routes for the same login.",
      ),
      defaultBody: `{
  "name": "my-account",
  "refresh_token": "<refresh-token>",
  "proxy_url": "",
  "custom_headers": {
    "Chatgpt-Account-Id": "workspace-id"
  }
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/api/admin/accounts \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "my-account",
  "refresh_token": "<refresh-token>\\n<another-refresh-token>",
  "proxy_url": "",
  "custom_headers": {
    "Chatgpt-Account-Id": "workspace-id"
  }
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "message": "成功添加 1 个账号",
  "success": 1,
  "failed": 0
}`,
        },
        { code: 400, body: `{"error": "refresh_token 是必填字段"}` },
        { code: 401, body: `{"error": "Unauthorized"}` },
      ],
    },
    {
      id: "admin-add-at",
      method: "POST",
      path: "/api/admin/accounts/at",
      title: copy(
        locale,
        "添加账号（Access Token）",
        "Add account (Access Token)",
      ),
      description: copy(
        locale,
        "添加 AT-only 账号；access_token 字段支持用换行分隔多个 Token。可通过 custom_headers.Chatgpt-Account-Id 指定目标工作区。",
        "Add AT-only accounts; the access_token field can contain multiple tokens separated by newlines. Use custom_headers.Chatgpt-Account-Id to select the target workspace.",
      ),
      defaultBody: `{
  "name": "at-account",
  "access_token": "eyJhbGciOi...",
  "proxy_url": "",
  "custom_headers": {
    "Chatgpt-Account-Id": "workspace-id"
  }
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/at \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "at-account",
  "access_token": "eyJhbGciOi...",
  "proxy_url": "",
  "custom_headers": {
    "Chatgpt-Account-Id": "workspace-id"
  }
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "message": "成功添加 1 个 AT-only 账号",
  "success": 1,
  "failed": 0
}`,
        },
        { code: 400, body: `{"error": "access_token 是必填字段"}` },
      ],
    },
    {
      id: "admin-import",
      method: "POST",
      path: "/api/admin/accounts/import",
      title: copy(locale, "文件批量导入账号", "Bulk import accounts from file"),
      description: copy(
        locale,
        "通过文件批量导入账号，支持 txt、CLIProxyAPI 导出的 json、以及每行一个 AT 的 at_txt，文件最大 20MB。",
        "Bulk import accounts from file. Supports txt, CLIProxyAPI-exported json, and at_txt with one access token per line; max file size is 20MB.",
      ),
      curl: `# TXT — one Refresh Token per line
curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --form 'file=@tokens.txt' \\
  --form 'format=txt' \\
  --form 'proxy_url='

# JSON — CLIProxyAPI credential export
curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --form 'file=@credentials.json' \\
  --form 'format=json' \\
  --form 'proxy_url='

# AT TXT — one Access Token per line
curl --request POST \\
  --url ${baseUrl}/api/admin/accounts/import \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --form 'file=@access_tokens.txt' \\
  --form 'format=at_txt' \\
  --form 'proxy_url='`,
      responses: [
        {
          code: 200,
          body: `{
  "message": "导入完成：成功 5，失败 0，重复 2",
  "total": 7,
  "success": 5,
  "failed": 0,
  "duplicate": 2
}`,
        },
        { code: 400, body: `{"error": "请上传文件（字段名: file）"}` },
      ],
    },
    {
      id: "admin-delete",
      method: "DELETE",
      path: "/api/admin/accounts/:id",
      title: copy(locale, "删除账号", "Delete account"),
      description: copy(
        locale,
        "按账号 ID 删除账号，并从可用号池中移除。",
        "Delete an account by ID and remove it from the active pool.",
      ),
      curl: `curl --request DELETE \\
  --url ${baseUrl}/api/admin/accounts/1 \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        { code: 200, body: `{"message": "账号已删除"}` },
        { code: 404, body: `{"error": "账号不存在"}` },
      ],
    },
    {
      id: "admin-list",
      method: "GET",
      path: "/api/admin/accounts",
      title: copy(locale, "列出账号", "List accounts"),
      description: copy(
        locale,
        "列出账号的状态、用量、标签、账号分组和基础元数据。可选 query channel=codex|grok|antigravity|claude 仅返回对应上游（Claude 管理页用 channel=claude）。",
        "List accounts with status, usage, tags, account groups, and basic metadata. Optional query channel=codex|grok|antigravity|claude returns only that upstream (use channel=claude for the Claude admin page).",
      ),
      curl: `curl --request GET \\
  --url ${baseUrl}/api/admin/accounts \\
  --header 'X-Admin-Key: <admin_secret>'

# Grok only
curl --request GET \\
  --url '${baseUrl}/api/admin/accounts?channel=grok' \\
  --header 'X-Admin-Key: <admin_secret>'

# Claude only
curl --request GET \\
  --url '${baseUrl}/api/admin/accounts?channel=claude' \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        {
          code: 200,
          body: `{
  "accounts": [
    {
      "id": 1,
      "name": "my-account",
      "email": "user@example.com",
      "plan_type": "team",
      "status": "active",
      "proxy_url": "",
      "tags": ["team"],
      "group_ids": [1],
      "allowed_api_key_ids": [],
      "created_at": "2025-01-01T00:00:00Z",
      "total_requests": 128,
      "success_requests": 125
    }
  ]
}`,
        },
      ],
    },
    {
      id: "admin-update-scheduler",
      method: "PATCH",
      path: "/api/admin/accounts/:id/scheduler",
      title: copy(
        locale,
        "更新账号调度配置",
        "Update account scheduler config",
      ),
      description: copy(
        locale,
        "更新账号代理、标签、账号分组、并发/评分覆盖和 API Key 反向授权。字段省略时保持原值；allowed_api_key_ids 传 null 或空数组表示不限制 API Key。",
        "Update account proxy, tags, account groups, concurrency/score overrides, and reverse API key authorization. Omitted fields keep existing values; allowed_api_key_ids set to null or [] means unrestricted API key access.",
      ),
      defaultBody: `{
  "proxy_url": "",
  "tags": ["team", "paid"],
  "group_ids": [1, 2],
  "allowed_api_key_ids": [],
  "score_bias_override": null,
  "base_concurrency_override": null,
  "skip_warm_tier": true,
  "keep_concurrency_on_degrade": false
}`,
      curl: `curl --request PATCH \\
  --url ${baseUrl}/api/admin/accounts/1/scheduler \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "tags": ["team", "paid"],
  "group_ids": [1, 2],
  "allowed_api_key_ids": [],
  "skip_warm_tier": true
}'`,
      responses: [
        { code: 200, body: `{"message": "账号调度配置已更新"}` },
        {
          code: 400,
          body: `{"error": "allowed_api_key_ids 包含不存在的 API Key ID: 99"}`,
        },
        { code: 404, body: `{"error": "账号不存在"}` },
      ],
    },
    {
      id: "admin-list-keys",
      method: "GET",
      path: "/api/admin/keys",
      title: copy(locale, "列出 API 密钥", "List API keys"),
      description: copy(
        locale,
        "列出后台创建的下游调用密钥，包含额度、用量、过期时间、状态和允许账号分组。该接口会在 raw_key 返回完整密钥，只能在受信任后台使用。",
        "List downstream API keys created in the admin panel, including quota, usage, expiration, status, and allowed account groups. The raw_key field contains the full secret and must only be used in trusted admin contexts.",
      ),
      curl: `curl --request GET \\
  --url ${baseUrl}/api/admin/keys \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        {
          code: 200,
          body: `{
  "keys": [
    {
      "id": 1,
      "name": "Claude Code",
      "key": "sk-****...abcd",
      "raw_key": "sk-live-full-key",
      "quota_limit": 10,
      "quota_used": 1.25,
      "expires_at": "2026-06-01T00:00:00Z",
      "allowed_group_ids": [1],
      "status": "active",
      "created_at": "2026-05-13T00:00:00Z"
    }
  ]
}`,
        },
      ],
    },
    {
      id: "admin-create-key",
      method: "POST",
      path: "/api/admin/keys",
      title: copy(locale, "创建 API 密钥", "Create API key"),
      description: copy(
        locale,
        "创建下游客户端使用的 API Key。key 可省略由系统生成；quota_limit 为 0 或省略表示不限额；allowed_group_ids 为空表示可调度全部账号分组。",
        "Create a downstream API key. The key can be generated automatically; quota_limit omitted or set to 0 means unlimited; empty allowed_group_ids means all account groups are allowed.",
      ),
      defaultBody: `{
  "name": "Claude Code",
  "quota_limit": 10,
  "expires_in_days": 30,
  "allowed_group_ids": [1]
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/api/admin/keys \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "Claude Code",
  "quota_limit": 10,
  "expires_in_days": 30,
  "allowed_group_ids": [1]
}'`,
      responses: [
        {
          code: 200,
          body: `{
  "id": 2,
  "key": "sk-...",
  "name": "Claude Code",
  "quota_limit": 10,
  "quota_used": 0,
  "expires_at": "2026-06-12T00:00:00Z",
  "allowed_group_ids": [1]
}`,
        },
        {
          code: 400,
          body: `{"error": "allowed_group_ids 包含不存在的分组 ID: 99"}`,
        },
      ],
    },
    {
      id: "admin-update-key",
      method: "PATCH",
      path: "/api/admin/keys/:id",
      title: copy(locale, "编辑 API 密钥", "Edit API key"),
      description: copy(
        locale,
        "编辑密钥名称、额度、过期时间和允许账号分组。字段省略时保持原值；quota_limit 传 0/null 清除额度；expires_at 传 null 或 expires_in_days 传 0 清除过期时间。",
        "Edit API key name, quota, expiration, and allowed account groups. Omitted fields keep existing values; quota_limit set to 0/null clears the limit; expires_at set to null or expires_in_days set to 0 clears expiration.",
      ),
      defaultBody: `{
  "name": "Cherry Studio",
  "quota_limit": 25,
  "expires_at": null,
  "allowed_group_ids": []
}`,
      curl: `curl --request PATCH \\
  --url ${baseUrl}/api/admin/keys/2 \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "name": "Cherry Studio",
  "quota_limit": 25,
  "expires_at": null,
  "allowed_group_ids": []
}'`,
      responses: [
        { code: 200, body: `{"message": "API Key 已更新"}` },
        { code: 400, body: `{"error": "额度限制不能小于 0"}` },
        { code: 404, body: `{"error": "API Key 不存在"}` },
      ],
    },
    {
      id: "admin-delete-key",
      method: "DELETE",
      path: "/api/admin/keys/:id",
      title: copy(locale, "删除 API 密钥", "Delete API key"),
      description: copy(
        locale,
        "删除 API Key 并立即让使用该密钥的客户端失去访问权限。",
        "Delete an API key and immediately revoke client access using it.",
      ),
      curl: `curl --request DELETE \\
  --url ${baseUrl}/api/admin/keys/2 \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [{ code: 200, body: `{"message": "已删除"}` }],
    },
    {
      id: "admin-list-groups",
      method: "GET",
      path: "/api/admin/account-groups",
      title: copy(locale, "列出账号分组", "List account groups"),
      description: copy(
        locale,
        "列出账号分组、颜色、描述和成员数量。",
        "List account groups with color, description, and member count.",
      ),
      curl: `curl --request GET \\
  --url ${baseUrl}/api/admin/account-groups \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        {
          code: 200,
          body: `{
  "groups": [
    {"id": 1, "name": "Team", "description": "付费团队账号", "color": "#2563eb", "member_count": 8, "sort_order": 0}
  ]
}`,
        },
      ],
    },
    {
      id: "admin-create-group",
      method: "POST",
      path: "/api/admin/account-groups",
      title: copy(locale, "创建账号分组", "Create account group"),
      description: copy(
        locale,
        "创建账号分组。账号可属于多个分组；API Key 可限制只能调度指定分组。",
        "Create an account group. Accounts may belong to multiple groups, and API keys may be restricted to specific groups.",
      ),
      defaultBody: `{
  "name": "Team",
  "description": "付费团队账号",
  "color": "#2563eb"
}`,
      curl: `curl --request POST \\
  --url ${baseUrl}/api/admin/account-groups \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{"name":"Team","description":"付费团队账号","color":"#2563eb"}'`,
      responses: [
        { code: 200, body: `{"id": 1, "message": "分组已创建"}` },
        { code: 400, body: `{"error": "分组名称不能为空"}` },
      ],
    },
    {
      id: "admin-update-group",
      method: "PATCH",
      path: "/api/admin/account-groups/:id",
      title: copy(locale, "编辑账号分组", "Edit account group"),
      description: copy(
        locale,
        "编辑账号分组名称、描述、颜色和排序。删除或改名分组后，账号和 API Key 的分组关系会按 ID 继续保持。",
        "Edit account group name, description, color, and sort order. Account and API key relations remain bound by group ID after rename operations.",
      ),
      defaultBody: `{
  "name": "Team Plus",
  "description": "高优先级账号",
  "color": "#16a34a",
  "sort_order": 10
}`,
      curl: `curl --request PATCH \\
  --url ${baseUrl}/api/admin/account-groups/1 \\
  --header 'X-Admin-Key: <admin_secret>' \\
  --header 'Content-Type: application/json' \\
  --data '{"name":"Team Plus","description":"高优先级账号","color":"#16a34a","sort_order":10}'`,
      responses: [
        { code: 200, body: `{"message": "分组已更新"}` },
        { code: 404, body: `{"error": "分组不存在"}` },
      ],
    },
    {
      id: "admin-delete-group",
      method: "DELETE",
      path: "/api/admin/account-groups/:id",
      title: copy(locale, "删除账号分组", "Delete account group"),
      description: copy(
        locale,
        "删除空分组。若分组仍有成员，需要追加 ?force=true；删除后会从账号关系中移除该 ID，并尽量从 API Key 允许分组中清理。若某个 API Key 仅绑定该分组，为避免权限被意外放大，会保留为缺失分组状态。",
        "Delete an empty account group. If the group still has members, append ?force=true; the ID is removed from account memberships and pruned from API key scopes where safe. If an API key only referenced this group, the missing group ID is retained so access does not silently broaden.",
      ),
      curl: `curl --request DELETE \\
  --url '${baseUrl}/api/admin/account-groups/1?force=true' \\
  --header 'X-Admin-Key: <admin_secret>'`,
      responses: [
        { code: 200, body: `{"message": "分组已删除"}` },
        { code: 409, body: `{"error": "分组仍有账号，确认后可强制删除"}` },
      ],
    },
  ];
}

function endpointToMd(e: EndpointSpec): string {
  const responses = e.responses
    .map(
      (r) =>
        `**${r.code}${r.label ? ` · ${r.label}` : ""}**\n\n\`\`\`${r.lang || "json"}\n${r.body}\n\`\`\``,
    )
    .join("\n\n");
  const parameters =
    e.parameters
      ?.map(
        (p) =>
          `- \`${p.name}\` (${p.type}${p.required ? ", required" : ""})${p.defaultValue ? ` [${p.defaultValue}]` : ""}: ${p.description}`,
      )
      .join("\n") || "";
  const examples =
    e.requestExamples
      ?.map((e) => `**${e.label}**\n\n\`\`\`${e.lang}\n${e.content}\n\`\`\``)
      .join("\n\n") || "";
  return `<a id="${e.id}"></a>\n\n### ${e.method} ${e.path} — ${e.title}\n\n${e.description}\n\n${parameters}\n\n\`\`\`bash\n${e.curl}\n\`\`\`\n\n${examples}\n\n${responses}`;
}

export function buildDocsMarkdown(args: {
  baseUrl: string;
  quickTools: QuickTool[];
  apiKeyExample: string;
  locale?: DocsLocale;
  clientConfigs?: { label: string; lang: string; content: string }[];
}): string {
  const {
    baseUrl,
    quickTools,
    apiKeyExample,
    locale = "zh",
    clientConfigs,
  } = args;
  const c = (zh: string, en: string) => copy(locale, zh, en);
  const guides = buildGuides(baseUrl, locale);
  const clients =
    clientConfigs ||
    quickTools
      .filter((tool) => tool.kind !== "protocol")
      .map((tool) => ({
        label: tool.name,
        lang: tool.templateLang || "text",
        content: resolveTemplate(tool, baseUrl, apiKeyExample),
      }));
  const block = (label: string, lang: string, content: string) =>
    `### ${label}\n\n\`\`\`${lang}\n${content}\n\`\`\``;
  return [
    `# ${c("Codex2API 使用文档", "Codex2API documentation")}`,
    `> ${c("服务地址", "Service URL")}: ${baseUrl}`,
    `## ${c("快速接入", "Quick start")}`,
    c(
      "1. 在账号管理中启用可用账号。\n2. 在 API 密钥中创建客户端密钥。\n3. 配置客户端并发送最小请求。\n4. 在使用统计核对结果与用量。",
      "1. Enable an eligible account.\n2. Create a client API key.\n3. Configure the client and send a minimal request.\n4. Check the result and usage in the dashboard.",
    ),
    ...clients.map((e) => block(e.label, e.lang, e.content)),
    `## ${c("开发指南", "Development")}`,
    ...guides.filter((g) => g.section === "development").map(guideToMarkdown),
    `## ${c("接口参考", "API reference")}`,
    ...guides.filter((g) => g.section === "model-api").map(guideToMarkdown),
    ...buildEndpointSpecs(baseUrl, locale).map(endpointToMd),
    `## ${c("故障排查", "Troubleshooting")}`,
    ...guides
      .filter((g) => g.section === "troubleshooting")
      .map(guideToMarkdown),
    `## ${c("管理与进阶", "Administration")}`,
    ...guides.filter((g) => g.section === "admin-api").map(guideToMarkdown),
    ...buildAdminSpecs(baseUrl, locale).map(endpointToMd),
  ].join("\n\n");
}
