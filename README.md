# ADS 动态配置中心

基于 GitHub 私有仓库 + MySQL 的分布式配置中心，支持多实例部署、配置热更新、本地缓存降级。

## 整体架构

```text
GitHub 私有仓库 (配置编辑入口)
       │
       ▼
Config Server 集群 (MySQL lease 选主，单实例同步)
       │
       ▼
MySQL (持久化存储)
       │
       ▼
Client SDK (本地缓存，轻量版本检查)
       │
       ▼
业务服务
```

**核心工作流**：
1. 运维在 GitHub 私有仓库编辑配置文件（JSON 格式），提交 PR 合入 main 分支
2. Config Server 集群中只有抢到 Leader 锁的实例定期拉取 GitHub 配置，写入 MySQL
3. MySQL 保存当前配置内容和全局版本号（GitHub commit SHA）
4. Client SDK 本地缓存配置，定期轻量检查版本号，有变化才全量刷新
5. MySQL 故障时 SDK 自动降级使用本地旧缓存，业务无感知

---

## MySQL 表说明

共 4 张表，均以 `ADS_SERVICE_DYNAMIC_CONFIG_` 开头。

### 1. ADS_SERVICE_DYNAMIC_CONFIG_CURRENT
配置主表，存储当前所有配置项的最新生效内容。

| 列名 | 类型 | 说明 |
|------|------|------|
| ID | BIGINT UNSIGNED | 自增主键 |
| NAMESPACE | VARCHAR(128) | 配置命名空间 |
| CONFIG_KEY | VARCHAR(256) | 配置文件名，与 NAMESPACE 组成联合唯一键 |
| PATH | VARCHAR(512) | GitHub 原始文件路径 |
| CONTENT | MEDIUMTEXT | 配置文件内容（JSON 字符串） |
| CONTENT_HASH | CHAR(64) | 内容 SHA256，相同内容不重复更新 |
| DELETED | TINYINT(1) | 删除标记，1 表示已在 GitHub 删除 |
| GITHUB_COMMIT_SHA | VARCHAR(64) | 对应 GitHub commit SHA |
| UPDATED_AT | DATETIME | 更新时间 |

- **写入方**：只有 Leader Server 同步 GitHub 配置时写入/更新
- **读取方**：Client SDK 初始化预热、版本变化时全量读取

### 2. ADS_SERVICE_DYNAMIC_CONFIG_META
元数据表，存储全局版本号和运行时配置参数。

| META_KEY | META_VALUE 说明 |
|----------|----------------|
| REPO_VERSION | 当前同步到的 GitHub commit SHA（**全局版本号，Client 轻量检查只查这一行**） |
| GITHUB_TOKEN | GitHub 私有仓库 PAT |
| GITHUB_OWNER | GitHub 仓库 owner，如 `chunhui204` |
| GITHUB_REPO | GitHub 仓库名，如 `ads-dynamic-config` |
| GITHUB_BRANCH | 分支名，默认 `main` |
| GITHUB_CONFIG_ROOT | 配置文件根目录，如 `configs` |
| GITHUB_BASE_URL | GitHub API 地址，默认 `https://api.github.com` |
| SYNC_INTERVAL | Server 同步间隔，如 `1m` |
| LOCK_LEASE_TTL | Leader 锁租约时长，如 `2m` |
| CLIENT_REFRESH_INTERVAL | Client 后台刷新检查间隔，如 `1m` |
| CLIENT_MAX_CACHE_TTL | Client 本地缓存最大有效期，如 `5m` |

- **写入方**：Leader Server 在一轮同步成功后更新 `REPO_VERSION`；其他 key 由运维预先插入
- **读取方**：
  - Server 启动时读取所有 GitHub/同步/Client 配置
  - Client 定期（TTL到期）只查 `REPO_VERSION` 一行判断是否需要刷新

### 3. ADS_SERVICE_DYNAMIC_CONFIG_SYNC_LEADER_LOCK
MySQL lease 选主锁表，保证同一时间窗口只有一个 Server 实例执行 GitHub 同步。

| 列名 | 类型 | 说明 |
|------|------|------|
| LOCK_NAME | VARCHAR(128) | 锁名，主键，固定值 `GITHUB_CONFIG_SYNC` |
| OWNER_ID | VARCHAR(128) | 当前持有锁的实例名（`INSTANCE_NAME` 环境变量） |
| EXPIRE_AT | DATETIME | 锁过期时间，过期后其他实例可抢锁 |
| UPDATED_AT | DATETIME | 最后更新时间 |

- **写入方**：所有 Server 实例抢锁/续约/释放锁时更新
- **选主原理**：通过 `UPDATE ... WHERE EXPIRE_AT < NOW()` 原子操作抢锁，影响行数为 1 则抢锁成功

### 4. ADS_SERVICE_DYNAMIC_CONFIG_SYNC_CHECKPOINT
同步检查点表，记录上一次成功同步的 GitHub commit，避免每轮全量扫描。

| 列名 | 类型 | 说明 |
|------|------|------|
| ID | BIGINT UNSIGNED | 自增主键 |
| OWNER | VARCHAR(128) | GitHub owner |
| REPO | VARCHAR(256) | GitHub repo |
| BRANCH | VARCHAR(128) | 分支名 |
| ROOT_PATH | VARCHAR(512) | 配置根目录 |
| ROOT_PATH_HASH | CHAR(64) | ROOT_PATH SHA256，用于联合唯一索引避免索引过长 |
| LAST_COMMIT_SHA | VARCHAR(64) | 上一次成功同步的 commit SHA |
| SYNCED_AT | DATETIME | 同步时间 |

- **写入方**：Leader Server 在一整轮同步（所有文件新增/更新/删除都完成）成功后才更新
- **读取方**：Leader Server 同步前读取，和当前 GitHub HEAD commit 对比，commit 没变直接跳过

---

## Server 端部署流程

### 第一步：建表
在 MySQL 中执行以下 SQL 创建 4 张表：

```bash
docker exec -e MYSQL_PWD='HertzUserPass123!' -i mysql-server mysql -hmysql-server -P3306 -uhertz_user hertz_db --table -e '
CREATE TABLE IF NOT EXISTS ADS_SERVICE_DYNAMIC_CONFIG_CURRENT (
    ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    NAMESPACE VARCHAR(128) NOT NULL,
    CONFIG_KEY VARCHAR(256) NOT NULL,
    PATH VARCHAR(512) NOT NULL,
    CONTENT MEDIUMTEXT NOT NULL,
    CONTENT_HASH CHAR(64) NOT NULL,
    DELETED TINYINT(1) NOT NULL DEFAULT 0,
    GITHUB_COMMIT_SHA VARCHAR(64) NOT NULL,
    UPDATED_AT DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (ID),
    UNIQUE KEY UK_NAMESPACE_KEY (NAMESPACE, CONFIG_KEY),
    KEY IDX_UPDATED_AT (UPDATED_AT)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ADS_SERVICE_DYNAMIC_CONFIG_META (
    META_KEY VARCHAR(128) NOT NULL,
    META_VALUE VARCHAR(512) NOT NULL,
    UPDATED_AT DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (META_KEY)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ADS_SERVICE_DYNAMIC_CONFIG_SYNC_LEADER_LOCK (
    LOCK_NAME VARCHAR(128) NOT NULL,
    OWNER_ID VARCHAR(128) NOT NULL,
    EXPIRE_AT DATETIME NOT NULL,
    UPDATED_AT DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (LOCK_NAME)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ADS_SERVICE_DYNAMIC_CONFIG_SYNC_CHECKPOINT (
    ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    OWNER VARCHAR(128) NOT NULL,
    REPO VARCHAR(256) NOT NULL,
    BRANCH VARCHAR(128) NOT NULL,
    ROOT_PATH VARCHAR(512) NOT NULL,
    ROOT_PATH_HASH CHAR(64) NOT NULL,
    LAST_COMMIT_SHA VARCHAR(64) NOT NULL,
    SYNCED_AT DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (ID),
    UNIQUE KEY UK_REPO_BRANCH_ROOT (OWNER, REPO, BRANCH, ROOT_PATH_HASH)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT IGNORE INTO ADS_SERVICE_DYNAMIC_CONFIG_SYNC_LEADER_LOCK
    (LOCK_NAME, OWNER_ID, EXPIRE_AT)
VALUES ("GITHUB_CONFIG_SYNC", "", "1970-01-01 00:00:00");
'
```

### 第二步：插入 META 配置

```sql
INSERT INTO ADS_SERVICE_DYNAMIC_CONFIG_META (META_KEY, META_VALUE)
VALUES
('GITHUB_TOKEN', '你的github_pat_token'),
('GITHUB_OWNER', 'chunhui204'),
('GITHUB_REPO', 'ads-dynamic-config'),
('GITHUB_BRANCH', 'main'),
('GITHUB_CONFIG_ROOT', 'configs'),
('GITHUB_BASE_URL', 'https://api.github.com'),
('SYNC_INTERVAL', '1m'),
('LOCK_LEASE_TTL', '2m'),
('CLIENT_REFRESH_INTERVAL', '1m'),
('CLIENT_MAX_CACHE_TTL', '5m')
ON DUPLICATE KEY UPDATE META_VALUE = VALUES(META_VALUE);
```

### 第三步：准备 config/server.json

复制模板并填入 MySQL 连接信息（`config/server.json` 已在 `.gitignore` 中，不会提交密码）：

```bash
cp config/server.json.example config/server.json
```

```json
{
  "database": {
    "type": "mysql",
    "host": "YOUR_MYSQL_HOST",
    "port": 3306,
    "user": "YOUR_MYSQL_USER",
    "password": "YOUR_MYSQL_PASSWORD",
    "db_name": "YOUR_MYSQL_DB",
    "max_open_conns": 100,
    "max_idle_conns": 20,
    "conn_max_lifetime": "30m"
  }
}
```

### 第四步：部署 Server

#### 方式 A：手动 Docker Run（单实例）

```bash
# 构建镜像
docker build -t ads-dynamic-config-server:latest .

# 启动容器
docker run -d \
  --name config-server-1 \
  --restart always \
  -p 8080:8080 \
  -e INSTANCE_NAME=config-server-1 \
  -v $(pwd)/config:/app/config \
  ads-dynamic-config-server:latest
```

#### 方式 B：手动 Docker Run（多实例，同一机器多端口）

```bash
# 实例1：宿主机 11999 -> 容器 8080
docker run -d \
  --name config-server-1 \
  --restart always \
  -p 11999:8080 \
  -e INSTANCE_NAME=config-server-1 \
  -v $(pwd)/config:/app/config \
  ads-dynamic-config-server:latest

# 实例2：宿主机 12000 -> 容器 8080
docker run -d \
  --name config-server-2 \
  --restart always \
  -p 12000:8080 \
  -e INSTANCE_NAME=config-server-2 \
  -v $(pwd)/config:/app/config \
  ads-dynamic-config-server:latest
```

MySQL 会自动选主，两个实例中只有一个会实际执行 GitHub 同步。

#### 方式 C：Docker Compose

编辑 `.env` 文件：
```bash
INSTANCE_NAME=config-server-1
HOST_PORT=8080
```

启动：
```bash
docker compose up -d --build
```

#### 方式 D：GitHub Actions 自动部署

1. 在代码仓库 Settings → Secrets and variables → Actions 中添加以下 Secrets：

   **服务器连接：**

   | Secret 名 | 说明 |
   |-----------|------|
   | DEPLOY_HOST | 部署服务器 IP |
   | DEPLOY_USER | SSH 用户名，如 `root` |
   | DEPLOY_SSH_KEY | SSH 私钥内容（`cat ~/.ssh/id_rsa`） |
   | DEPLOY_PORT | SSH 端口，默认 22（可选） |
   | DEPLOY_PORTS | 宿主机端口列表，逗号分隔，如 `11999,12000`，每个端口启动一个容器实例（可选，默认 `8080`） |

   **MySQL 连接（部署时自动生成 config/server.json，不会提交到仓库）：**

   | Secret 名 | 说明 |
   |-----------|------|
   | MYSQL_HOST | MySQL 主机地址 |
   | MYSQL_PORT | MySQL 端口，如 `3306` |
   | MYSQL_USER | MySQL 用户名 |
   | MYSQL_PASSWORD | MySQL 密码 |
   | MYSQL_DB | MySQL 数据库名 |

2. 代码推送到 `master` 分支自动触发部署，workflow 会：
   - scp 代码到服务器 `/opt/ads-dynamic-config`
   - 根据 MySQL Secrets 动态生成 `config/server.json`
   - docker build 构建镜像
   - 按 `DEPLOY_PORTS` 端口数量启动对应数量的容器，实例名自动为 `config-server-1`、`config-server-2`...

> **注意：** `config/server.json` 已加入 `.gitignore`，不会将数据库密码提交到代码仓库。本地开发时可参考 `config/server.json.example` 创建该文件。

### 第五步：验证部署

```bash
# 健康检查
curl http://服务器IP:8080/health
# 返回 ok

# 查看日志
docker logs -f config-server-1
# 正常输出：
# mysql connected: 8.222.145.95:3306/hertz_db
# config sync server started, instance_name=config-server-1, sync_interval=1m0s, lock_ttl=2m0s
# http server listening on :8080
```

---

## Client SDK 使用方法

### 1. 引入依赖

```go
import "github_based_config_proxy/client_sdk"
import _ "github.com/go-sql-driver/mysql"
```

### 2. 定义配置结构体

根据 GitHub 配置仓库中的 JSON 文件定义对应 Go 结构体：

```go
// payment/risk.json 对应结构体
type RiskConfig struct {
    Enabled bool  `json:"enabled"`
    Limit   int64 `json:"limit"`
}

// common/whitelist.json 对应字符串数组
// ["user1", "user2", "user3"]

// common/blacklist.json 对应对象数组
type BlacklistItem struct {
    ID   int64  `json:"id"`
    Name string `json:"name"`
}

// ad/channels.json 对应 Map
type ChannelConfig struct {
    Weight int `json:"weight"`
}
```

### 3. 初始化 SDK（完整示例）

```go
package main

import (
    "context"
    "database/sql"
    "log"
    "net/http"
    "os"
    "os/signal"
    "strconv"
    "syscall"
    "time"

    "github_based_config_proxy/client_sdk"

    _ "github.com/go-sql-driver/mysql"
)

// 全局配置句柄
var (
    configClient *client_sdk.Client

    // 类型安全配置句柄，全局只注册一次
    riskCfg      *client_sdk.TypedConfig[RiskConfig]
    whitelistCfg *client_sdk.TypedConfig[[]string]
    blacklistCfg *client_sdk.TypedConfig[[]BlacklistItem]
    channelsCfg  *client_sdk.TypedConfig[map[string]ChannelConfig]
)

func initConfig() error {
    dsn := "hertz_user:HertzUserPass123!@tcp(8.222.145.95:3306)/hertz_db?parseTime=true"

    // ========== 创建 client（二选一） ==========
    // 方式1：SDK 内部创建并管理连接池（简单场景）
    var err error
    configClient, err = client_sdk.NewClientWithDSN(dsn)
    if err != nil {
        return err
    }

    // 方式2：复用业务已有的 *sql.DB（推荐生产环境，统一连接池管理）
    // db, err := sql.Open("mysql", dsn)
    // if err != nil { return err }
    // db.SetMaxOpenConns(100)
    // db.SetMaxIdleConns(20)
    // db.SetConnMaxLifetime(30 * time.Minute)
    // configClient, err = client_sdk.NewClient(client_sdk.Config{DB: db})
    // if err != nil { return err }

    // ========== 注册类型安全配置（Init 前后都可以注册） ==========
    riskCfg = client_sdk.Register[RiskConfig](configClient, "payment", "risk.json")
    whitelistCfg = client_sdk.Register[[]string](configClient, "common", "whitelist.json")
    blacklistCfg = client_sdk.Register[[]BlacklistItem](configClient, "common", "blacklist.json")
    channelsCfg = client_sdk.Register[map[string]ChannelConfig](configClient, "ad", "channels.json")

    // ========== 阻塞预热：从 MySQL 全量加载配置，加载完才算启动完成 ==========
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := configClient.Init(ctx); err != nil {
        return err
    }

    // ========== 启动后台自动刷新 goroutine ==========
    go func() {
        if err := configClient.Start(context.Background()); err != nil {
            log.Printf("config client refresh stopped: %v", err)
        }
    }()

    return nil
}

func main() {
    // 初始化配置
    if err := initConfig(); err != nil {
        log.Fatalf("init config failed: %v", err)
    }
    defer configClient.Close()

    // ========== HTTP 服务中使用配置 ==========
    mux := http.NewServeMux()

    // 示例1：使用类型安全配置，零反序列化开销
    mux.HandleFunc("/check-risk", func(w http.ResponseWriter, r *http.Request) {
        cfg, ok := riskCfg.Get()
        if !ok {
            http.Error(w, "config not found", http.StatusInternalServerError)
            return
        }
        if cfg.Enabled {
            w.Write([]byte("risk check enabled, limit=" + strconv.FormatInt(cfg.Limit, 10)))
        } else {
            w.Write([]byte("risk disabled"))
        }
    })

    // 示例2：遍历字符串数组配置
    mux.HandleFunc("/whitelist", func(w http.ResponseWriter, r *http.Request) {
        wl, ok := whitelistCfg.Get()
        if !ok {
            http.Error(w, "config not found", http.StatusInternalServerError)
            return
        }
        for _, item := range wl {
            _, _ = w.Write([]byte(item + "\n"))
        }
    })

    // 示例3：获取原始字符串（兼容旧逻辑）
    mux.HandleFunc("/raw", func(w http.ResponseWriter, r *http.Request) {
        raw := configClient.GetConfig("payment", "risk.json")
        _, _ = w.Write([]byte(raw))
    })

    // 启动 HTTP 服务
    server := &http.Server{Addr: ":8080", Handler: mux}
    go func() {
        log.Printf("business server starting on :8080")
        if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Fatalf("server error: %v", err)
        }
    }()

    // 优雅关闭
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit
    log.Printf("shutting down...")
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = server.Shutdown(ctx)
}
```

### 4. SDK 关键特性说明

| 特性 | 说明 |
|------|------|
| **启动预热** | `Init()` 阻塞从 MySQL 全量加载配置，加载完成才算初始化完成，保证业务启动后第一时间能拿到配置 |
| **轻量版本检查** | 后台每隔 `CLIENT_REFRESH_INTERVAL` 检查一次，TTL 到期后只查 `META` 表一行 `REPO_VERSION`，无配置变化不查配置表 |
| **零反序列化开销** | `Register[T]` 注册的类型配置，只在版本变化时反序列化一次，`Get()` 直接取预解析好的对象，业务调用无任何解析开销 |
| **自动降级** | 运行期 MySQL 查询失败时继续使用本地旧缓存，不影响业务读取；只有启动阶段 MySQL 不可用才会 Init 失败 |
| **支持任意 JSON 类型** | 支持普通结构体、结构体数组、字符串数组、Map 等所有 `json.Unmarshal` 支持的类型 |
| **并发安全** | 配置快照整体原子替换，业务调用无锁，支持高并发读取 |
| **只读对象** | `Get()` 返回的对象不要修改，否则会污染缓存 |

### 5. 配置文件路径映射规则

`namespace + config_key` 对应 GitHub 仓库中 `GITHUB_CONFIG_ROOT` 目录下的文件路径：

```text
GitHub 路径                          namespace        config_key
─────────────────────────────────────────────────────────────────
configs/payment/risk.json      ->    payment          risk.json
configs/common/whitelist.json  ->    common           whitelist.json
configs/ad/channels.json       ->    ad               channels.json
configs/a/b/c.json             ->    a/b              c.json
```

---

## 配置仓库 JSON 校验

配置仓库（如 `chunhui204/ads-dynamic-config`）需要放置 `.github/workflows/config-repo-validate.yml` 文件，PR/push 时自动校验所有配置文件是否为合法 JSON，格式错误的配置无法合入。

workflow 文件内容见本仓库 [.github/workflows/config-repo-validate.yml](.github/workflows/config-repo-validate.yml)。

---

## 目录结构

```text
.
├── cmd/
│   ├── server/          # Config Server 主入口
│   └── local_verify/    # 本地验证工具
├── server/              # Server 同步逻辑、MySQL 存储、GitHub API 客户端
├── client_sdk/          # Go Client SDK
├── config/
│   ├── server.json.example   # MySQL 连接配置模板（参考）
│   └── server.json           # 实际配置文件（.gitignore，不提交到仓库）
├── .github/workflows/
│   ├── deploy.yml                  # Server 自动部署 workflow
│   └── config-repo-validate.yml    # 配置仓库 JSON 校验 workflow（复制到配置仓库）
├── Dockerfile
├── docker-compose.yml
└── README.md
```

## 环境变量依赖

| 环境变量 | 必填 | 说明 |
|---------|------|------|
| `INSTANCE_NAME` | 是 | Server 容器唯一实例名，多实例部署必须唯一；Client SDK 不依赖任何环境变量 |
