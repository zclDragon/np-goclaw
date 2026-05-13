# 本地前后端启动

## 适用场景

这份说明适用于：

- 前端本机开发：`http://localhost:5173`
- 后端本机开发：`http://localhost:18790`
- 数据库使用 Docker 启动的 Postgres

## 前置依赖

需要本机具备：

- Go
- Node.js
- pnpm
- Docker Desktop

可用下面命令自检：

```bash
go version
node -v
pnpm -v
docker version
```

## 1. 启动数据库

在项目根目录执行：

```bash
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d postgres
```

确认数据库健康：

```bash
docker compose -f docker-compose.yml -f docker-compose.postgres.yml ps postgres
```

正常情况下会看到：

```bash
Up ... (healthy)
```

数据库默认连接信息：

- Host: `localhost`
- Port: `5432`
- Database: `goclaw`
- User: `goclaw`
- Password: `goclaw`

如果你在 `.env` 中自定义了 `POSTGRES_PASSWORD`，请以你的实际值为准。

## 2. 配置后端环境变量

项目根目录需要有 `.env` 文件。

本地开发至少确认这些变量存在：

```env
GOCLAW_GATEWAY_TOKEN=your-token
GOCLAW_POSTGRES_DSN=postgres://goclaw:123456@localhost:5432/goclaw?sslmode=disable
```

注意：

- `make run` 不会自动读取根目录 `.env`
- 需要先把 `.env` 导入当前 shell，再启动后端

推荐方式：

```bash
set -a
source .env
set +a
```

## 3. 启动前端开发服务器

前端使用 Vite，默认端口是 `5173`。

先安装依赖：

```bash
cd ui/web && pnpm install --frozen-lockfile
```

如果本机后端跑在 `18790`，需要确保 `ui/web/.env` 里有：

```env
VITE_BACKEND_PORT=18790
```

启动前端：

```bash
make dev
```

打开：

```text
http://localhost:5173
```

说明：

- `/v1`
- `/ws`
- `/health`

会由 Vite 代理到后端。

## 4. 启动本机后端

### 只跑后端 API

在项目根目录执行：

```bash
set -a; source .env; set +a; make run
```

这会启动后端 API，但不会重新打包前端静态资源。

适合场景：

- 前端走 `make dev`
- 后端只提供接口

后端启动后默认可访问：

```text
http://localhost:18790
```

健康检查：

```bash
curl http://localhost:18790/health
```

## 5. 如果你要让 18790 直接显示最新前端页面

如果你改了 `ui/web/src` 里的页面，并且希望：

```text
http://localhost:18790/login
```

也显示最新改动，就不能只用 `make run`。

因为 `make run` 只会构建后端，不会把最新前端打进二进制。

正确方式：

```bash
cd ui/web && pnpm build
cd ../..
make build-full
set -a; source .env; set +a; ./goclaw
```

说明：

- `pnpm build` 生成最新前端 dist
- `make build-full` 把 dist 嵌入 Go 二进制
- `./goclaw` 启动带内嵌前端的后端

## 6. 常见问题

### 6.1 登录页提示凭据无效

先确认后端启动时是否真的带上了 `.env`。

如果只是这样启动：

```bash
make run
```

那么 `GOCLAW_GATEWAY_TOKEN` 很可能没有进进程。

应改为：

```bash
set -a; source .env; set +a; make run
```

### 6.2 为什么在根目录执行 `make run` 也不行

因为：

- 当前目录是根目录
- 不代表 shell 会自动加载 `.env`

`make run` 只是执行：

```bash
./goclaw
```

不会自动 `source .env`。

### 6.3 为什么 5173 上改动生效，18790 上不生效

因为：

- `5173` 是 Vite dev server，源码热更新
- `18790` 是后端提供的嵌入式前端，需要重新构建

## 7. 最常用命令

### 本机联调推荐

```bash
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d postgres
cd ui/web && pnpm install --frozen-lockfile
cd ../..
set -a; source .env; set +a
make dev
```

另开一个终端：

```bash
cd /Users/zhuchenglong/workspace/open-source-projects/np-goclaw
set -a; source .env; set +a; make run
```

### 带最新嵌入式前端启动

```bash
docker compose -f docker-compose.yml -f docker-compose.postgres.yml up -d postgres
cd ui/web && pnpm build
cd ../..
set -a; source .env; set +a; make build-full && ./goclaw
```
