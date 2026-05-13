# 24 - Knowledge Vault

Knowledge Vault 是现有记忆系统（情节记忆、知识图谱、记忆文件）之上的查询层。它提供文档注册、双向 wikilink、文件系统同步，以及跨所有知识源的统一搜索。

**不是替代品** —— 它是在现有能力之上做扩展。Vault 位于 agent 与 episodic/KG 存储之间，使 agent 能以显式关系整理工作区文档。

---

## 1. 架构概览

### 组件

| 组件 | 作用 |
|-----------|------|
| **VaultStore** | 接口：文档 CRUD、链接管理、混合搜索（FTS+vector） |
| **VaultService** | 搜索协调器：并行扇出到 vault、episodic、KG，并做加权排序 |
| **VaultSyncWorker** | 文件系统监听器：检测文件变化（创建/写入/删除），把内容哈希同步回注册表 |
| **VaultRetriever** | 检索适配器：把 vault search 接入 agent 的 L0 memory 系统 |
| **HTTP Handlers** | REST 接口：列表、获取、搜索、链接 |

### 数据流

```
Agent 写入文档 → Workspace 文件系统
                 ↓
          VaultSyncWorker 检测变化
                 ↓
      更新 vault_documents（哈希、元数据）
                 ↓
      当 agent 发起查询：vault_search 工具
                 ↓
     VaultSearchService（并行扇出）
          ↙        ↓        ↘
      Vault    Episodic    Knowledge Graph
          ↘        ↓        ↙
        归一化并加权分数
                 ↓
          返回 Top 结果
```

### 租户与作用域隔离

文档按 **tenant**（隔离）、**agent**（每个 agent 的命名空间）和**文档作用域**（personal/team/shared）进行划分：

- **personal**：agent 专属文档（agent_context_files、每用户工作内容）
- **team**：团队工作区文档（team_context_files）
- **shared**：跨租户共享知识（未来能力）

### 文档作用域与归属

| scope | agent_id | team_id | 可见性 |
|-------|----------|---------|------------|
| personal | set  | NULL   | 仅所属 agent 可见（tenant 内） |
| team     | NULL | set    | 团队成员可见（tenant 内） |
| shared   | NULL | NULL   | tenant 内所有 agent 可见 |
| custom   | any  | any    | 通过 `custom_scope` 自定义 |

#### DB 不变量（migration 000055）

一个 CHECK 约束会强制执行上面的表：`vault_documents_scope_consistency`。任何违反 scope 与 ownership 关系的插入/更新都会被拒绝。`scope='custom'` 不受约束（用户自定义作用域）。

#### Agent 读取语义

`vault_search`、`ListDocuments`、`CountDocuments` 会返回：
- 该 agent 拥有的文档（`agent_id = <agent>`）
- 加上共享文档（`agent_id IS NULL`）

在团队上下文中（RunContext 设置了 TeamID），结果还会包含该团队的 team 作用域文档。始终强制 tenant 隔离（`tenant_id = <tenant>`）。

---

## 2. 数据模型

### vault_documents

文档注册表：存储元数据指针。内容本身保存在文件系统；注册表保存 path、hash、embedding 和 links。

| 列 | 类型 | 说明 |
|--------|------|-------|
| `id` | UUID | 主键 |
| `tenant_id` | UUID | 多租户隔离 |
| `agent_id` | UUID | 每个 agent 的命名空间 |
| `scope` | TEXT | personal \| team \| shared |
| `path` | TEXT | 相对 workspace 的路径（workspace/notes/foo.md） |
| `title` | TEXT | 展示名称 |
| `doc_type` | TEXT | context、memory、note、skill、episodic |
| `content_hash` | TEXT | 文件内容的 SHA-256（用于检测变化） |
| `embedding` | vector(1536) | pgvector：语义相似度 |
| `tsv` | tsvector | 生成列：title+path 的 FTS 索引 |
| `metadata` | JSONB | 可选自定义字段 |
| `created_at`, `updated_at` | TIMESTAMPTZ | 时间戳 |
| **唯一约束** | (agent_id, scope, path) | 每个 scope 下同一路径仅一份文档 |

**索引：**
- `idx_vault_docs_tenant` — tenant_id（多租户查询）
- `idx_vault_docs_agent_scope` — agent_id, scope（agent 范围过滤）
- `idx_vault_docs_type` — agent_id, doc_type（类型过滤）
- `idx_vault_docs_hash` — content_hash（变化检测）
- `idx_vault_docs_embedding` — HNSW vector（语义搜索）
- `idx_vault_docs_tsv` — GIN FTS 索引（关键词搜索）

### vault_links

文档之间的双向链接（wikilink、显式引用等）。

| 列 | 类型 | 说明 |
|--------|------|-------|
| `id` | UUID | 主键 |
| `from_doc_id` | UUID | 源文档 |
| `to_doc_id` | UUID | 目标文档 |
| `link_type` | TEXT | wikilink、reference 等 |
| `context` | TEXT | 约 50 个字符：周边文本片段 |
| `created_at` | TIMESTAMPTZ | 创建时间 |
| **唯一约束** | (from_doc_id, to_doc_id, link_type) | 避免重复链接 |

**索引：**
- `idx_vault_links_from` — from_doc_id（出链）
- `idx_vault_links_to` — to_doc_id（反向链接）

### vault_versions

版本历史（为 v3.1+ 预留；在 v3.0 中为空）。

| 列 | 类型 | 说明 |
|--------|------|-------|
| `id` | UUID | 主键 |
| `doc_id` | UUID | 文档引用 |
| `version` | INT | 版本号 |
| `content` | TEXT | 文档内容快照 |
| `changed_by` | TEXT | 用户/agent 标识 |
| `created_at` | TIMESTAMPTZ | 快照时间 |

---

## 3. Wikilinks

`[[target]]` 格式的双向 Markdown 链接。

### 解析与提取

`ExtractWikilinks(content string)` 会解析所有 `[[...]]` 模式：

- **格式：** `[[path/to/file.md]]` 或 `[[name|display text]]`（display text 会被忽略）
- **返回：** `[]WikilinkMatch`，包含 target、context（约 50 字符）和字节偏移量

示例：
```markdown
See [[architecture/components]] for details.
Reference [[SOUL.md|agent persona]] here.
Link [[../parent-project]] up.
```

**边界情况：**
- 空目标 `[[]]` → 跳过
- 仅空白的目标 → trim 后跳过
- 含空格路径：`[[foo bar]]` → 保留
- 文件扩展名：如果缺少 `.md` 会自动补上

### 解析策略

`ResolveWikilinkTarget()` 用来找到与 wikilink target 对应的文档：

1. **精确路径匹配** —— `GetDocument(tenantID, agentID, "path/to/file.md")`
2. **补 `.md` 后重试** —— 如果 target 没有 `.md`，则再查一次
3. **按 basename 搜索** —— 线性扫描该 agent 的全部文档，按 basename 不区分大小写匹配
4. **未解析到** —— 返回 nil（不是错误；backlink 可以不完整）

例子：`[[SOUL.md]]` → 查 SOUL.md → 回退查 SOUL → 再扫描 basename 为 SOUL 的文档

### 链接同步

`SyncDocLinks(ctx, vaultStore, doc, content, tenantID, agentID)` 会让 vault_links 与文档内容中的 wikilink 保持同步：

1. 从内容中提取 `[[...]]`
2. 删除该文档所有出链（替换式策略）
3. 对每个匹配项：
   - 按上面的策略解析目标
   - 若成功解析，则创建 `vault_link` 记录
4. 返回（错误只记日志，不向外抛）

调用时机：
- 文档 upsert 时（agent 往 workspace 写入）
- VaultSyncWorker 处理文件变更时

---

## 4. 搜索

混合搜索把 vault FTS、向量 embedding、episodic memory 和 knowledge graph 结合起来。

### Vault 搜索（Store 层）

`VaultStore.Search(ctx, opts VaultSearchOptions)` 针对单个 vault：

- **FTS**：PostgreSQL `plainto_tsquery()`，基于 tsv（title+path 关键词）
- **Vector**：pgvector 余弦相似度（语义搜索）
- **组合评分**：先把每种方法的分数归一化到 0–1，再按查询时权重组合
- **结果：** 返回分数最高的 Top N 文档

### 统一搜索（跨存储）

`VaultSearchService.Search(ctx, opts UnifiedSearchOptions)`：

**并行扇出：**
```
Query → ├─ VaultStore.Search()      [0.4 权重]
        ├─ EpisodicStore.Search()   [0.3 权重]
        └─ KGStore.SearchEntities() [0.3 权重]
                ↓
         各来源先做归一化
         （最大分数 = 1.0，再乘权重）
                ↓
         合并并按 ID 去重
                ↓
         按最终分数降序排序
                ↓
         返回 top N
```

**分数归一化：** 每个来源的分数先缩放到 0–1（max_score / weight），再乘以各自权重：
- Vault：0.4（关键词 + 语义）
- Episodic：0.3（会话摘要）
- KG：0.3（实体关系）

每个来源默认最大返回数：`maxResults * 2`（随后去重，再裁剪到 maxResults）。

### 参数

| 参数 | 类型 | 默认值 | 说明 |
|-------|------|---------|-------|
| `Query` | string | — | 必填：自然语言 |
| `AgentID` | string | — | 限定到 agent |
| `TenantID` | string | — | 限定到 tenant |
| `Scope` | string | all | personal、team、shared |
| `DocTypes` | []string | all | context、memory、note、skill、episodic |
| `MaxResults` | int | 10 | 最终结果集数量 |
| `MinScore` | float64 | 0.0 | 最低分数过滤 |

---

## 5. 文件系统同步

`VaultSyncWorker` 监听 workspace 目录变化，并保持 vault_documents 中的 hash 与文件内容一致。

### Watcher 循环

使用 `fsnotify` 检测 Write、Create、Remove 事件：

1. 防抖：500ms（多次快速改动 → 合并成一批）
2. 对每个文件变化：
   - 计算文件内容的 SHA-256 哈希
   - 与 `vault_documents.content_hash` 比较
   - 若不同：更新数据库中的 hash
   - 若文件已删除：标记 `metadata["deleted"] = true`

### 约束

- 只同步**已注册**的文档（即已经进入 vault_documents 的文件）
- 新文件必须先由 agent 注册（通常通过 agent write）
- 不可读文件 → 标记为已删除

### 初始化

```go
syncer := vault.NewVaultSyncWorker(vaultStore)
go syncer.Watch(ctx, workspaceDir, tenantID, agentID)
```

---

## 6. HTTP API

Vault 操作的 REST 接口。

### 文档列表

**接口：** `GET /v1/agents/{agentID}/vault/documents`

**Query 参数：**
- `scope` — personal、team 或 shared（可选）
- `doc_type` — 逗号分隔的类型（可选）
- `limit` — 默认 20，最大 500
- `offset` — 分页

**响应：**
```json
[
  {
    "id": "uuid",
    "agent_id": "uuid",
    "path": "workspace/notes/foo.md",
    "title": "Foo Notes",
    "doc_type": "note",
    "content_hash": "sha256hex",
    "created_at": "2026-04-07T00:00:00Z"
  }
]
```

### 获取文档

**接口：** `GET /v1/agents/{agentID}/vault/documents/{docID}`

**响应：** 单个 VaultDocument 对象

### 搜索

**接口：** `POST /v1/agents/{agentID}/vault/search`

**请求体：**
```json
{
  "query": "authentication flow",
  "scope": "team",
  "doc_types": ["context", "note"],
  "max_results": 10
}
```

**响应：**
```json
[
  {
    "document": { /* VaultDocument */ },
    "score": 0.87,
    "source": "vault"
  },
  {
    "document": { /* episodic record */ },
    "score": 0.65,
    "source": "episodic"
  }
]
```

### 获取链接

**接口：** `GET /v1/agents/{agentID}/vault/documents/{docID}/links`

**响应：**
```json
{
  "outlinks": [
    {
      "id": "uuid",
      "to_doc_id": "uuid",
      "link_type": "wikilink",
      "context": "See [[target]] for details."
    }
  ],
  "backlinks": [
    {
      "id": "uuid",
      "from_doc_id": "uuid",
      "link_type": "wikilink",
      "context": "Reference [[SOUL.md]] here."
    }
  ]
}
```

### 列表（跨 Agent）

**接口：** `GET /v1/vault/documents`

**Query 参数：**
- `agent_id` — 可选，过滤到指定 agent（否则返回 tenant 下所有 agent）
- `scope`、`doc_type`、`limit`、`offset` — 与 per-agent 接口一致

---

## 7. Tools

Agent 通过两个工具访问 vault。

### vault_search

**主要发现工具：** 跨所有知识源统一搜索并排序。

**参数：**
```json
{
  "query": "string（必填）",
  "scope": "string（可选：personal|team|shared）",
  "types": "string（可选：逗号分隔的 doc type）",
  "maxResults": "number（可选，默认 10）"
}
```

**示例：**
```
Agent: "Find documents about authentication"
Tool call: vault_search(query="authentication", types="context,note")
Result: 返回来自 vault + episodic + KG 的前 10 条结果
```

### vault_link

**创建显式链接：** 把两个文档连接起来（类似 `[[wikilink]]`）。

**参数：**
```json
{
  "from": "string（必填，源文档路径）",
  "to": "string（必填，目标文档路径）",
  "context": "string（可选，关系说明）"
}
```

**示例：**
```
Agent: "Link the authentication guide to the SOUL file"
Tool call: vault_link(from="docs/auth.md", to="SOUL.md", context="Persona reference")
Result: 在 vault_links 中创建显式链接
```

---

## 8. Retriever Integration

`VaultRetriever` 用来把 vault search 接到 agent 的 L0 memory 系统里。

### 用法

```go
retriever := vault.NewVaultRetriever(searchService)
summaries, err := retriever.RetrieveL0(ctx, agentID, userID, query, config)
// 返回相关性最高的 []memory.L0Summary
```

### 配置

```go
type RetrieverConfig struct {
  RelevanceThreshold float64  // 默认 0.3
  MaxL0Items         int      // 默认 5
  TenantID           string
}
```

它用于 agent 的 think 阶段，在 plan/act 之前先取回相关文档。

---

## 9. Web UI (v3)

Dashboard 中的 Vault 页面会展示：

- **Document list** —— 按 scope/type 过滤的 workspace 文档列表
- **Graph visualization** —— 节点是文档，边是 wikilink，支持交互式平移/缩放
- **Search interface** —— 带来源标识的统一搜索界面
- **Link editor** —— 创建/删除文档间链接

---

## 10. Feature Flags & Configuration

Knowledge Vault 是 **仅 v3 提供** 的功能。

- **Edition:** Standard 和 Lite（完整支持）
- **Prerequisite:** PostgreSQL + pgvector 扩展
- **Storage:** 由 migration 000038 创建的 `vault_*` 表
- **Workspace:** 文档组织在 agent 级别的 workspace 目录下（例如 `~/.goclaw/workspace/agent_name/`）

没有显式 feature flag；满足以下条件就会启用 vault：
1. migration 000038 成功执行
2. gateway 启动时初始化了 VaultStore
3. 启动了 VaultSyncWorker

---

## 11. 示例

### 添加文档

Agent 向 workspace 写入：
```
~/.goclaw/workspace/myagent/notes/architecture.md
```

下一次同步时（或立即写入后）：
1. VaultSyncWorker 检测到文件创建
2. 计算 SHA-256 hash
3. 自动把文档注册进 vault，并写入元数据

### 创建 Wikilink

Markdown 内容：
```markdown
See [[architecture.md]] for system design.
See [[SOUL.md|persona]] for agent personality.
```

Agent 调用 `vault_search("system design")`：
1. ExtractWikilinks 找到 `[[architecture.md]]` 和 `[[SOUL.md]]`
2. ResolveWikilinkTarget 解析到已注册文档
3. SyncDocLinks 创建 vault_link 记录
4. 可通过 `/links` 接口拿到 backlinks

### 搜索示例

Agent: "Find notes about authentication"

请求：
```json
POST /v1/agents/agent-123/vault/search
{
  "query": "authentication flow",
  "scope": "personal",
  "max_results": 5
}
```

响应（并行汇总 vault + episodic + KG 的结果）：
```json
[
  {
    "document": {
      "id": "doc-456",
      "path": "notes/auth.md",
      "title": "Authentication Flow",
      "doc_type": "note"
    },
    "score": 0.92,
    "source": "vault"
  },
  {
    "id": "episodic-789",
    "title": "Session-2026-04-06",
    "source": "episodic",
    "score": 0.68
  }
]
```

---

## 12. 限制

- **Vault 文档不会自动嵌入 system prompt** —— 必须通过 agent 工具检索
- **不对文档正文做全文索引** —— FTS 只覆盖 title+path；正文依赖 embedding
- **同步是单向的** —— 文件系统变化会同步到 vault；vault 不会反向写回文件系统
- **没有冲突解决** —— 并发编辑不会检测，最后写入者覆盖前者
- **版本历史为空** —— `vault_versions` 表是为 v3.1 预留的

---

## 文件参考

| 模块 | 路径 | 用途 |
|---|---|---|
| Vault service & sync | `internal/vault/` | VaultStore、VaultService、VaultSyncWorker、VaultRetriever、wikilink 解析 |
| Store & HTTP | `internal/store/vault_store.go`, `internal/http/vault_handlers.go` | Store 接口、REST 接口（list、get、search、links） |
| Tools & migration | `internal/tools/vault_*.go`, `migrations/000038_vault_tables.up.sql` | vault_search / vault_link 工具、schema migration |

可以用 `grep` 或编辑器符号搜索继续查具体文件。
