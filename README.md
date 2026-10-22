## 快速开始

需要 账号。部署完成后，在面板登录或导入账号即可使用。

### 方式一：Docker Compose

安装 Docker 和 Compose 后执行：

```bash
git clone https://github.com/csszyft/wb2api.git
cd wb2api
docker compose pull
docker compose up -d
```

仓库中的 [docker-compose.yml](docker-compose.yml) 使用已构建的 `ghcr.io/csszyft/wb2api:latest`，**无需本地编译，也无需先复制 config.example.json**。需要固定版本时，将 Compose 的 `image` 改为 `ghcr.io/csszyft/wb2api:v1.9.45`。

首次启动会在宿主机 `./data/config.json` 生成配置和随机 `api_key`。打开该文件查看密钥，然后访问：

- 面板：`http://127.0.0.1:7863/panel/`
- 客户端 API Base URL：`http://127.0.0.1:7863/v1`
- 客户端 API Key：配置中的 `api_key`，与面板使用同一密钥

部署在服务器时，将 `127.0.0.1` 换成实际可访问的服务器地址或反向代理域名。

```bash
docker compose logs -f                  # 查看日志
docker compose restart                 # 重启现有容器
docker compose pull                    # 拉取新镜像
docker compose up -d                   # 使用新镜像重建服务，保留数据卷
docker compose down                    # 停止并移除容器，保留 ./data
```

### 方式二：Docker 命令

以下命令适用于 Bash / Linux / macOS；Windows 推荐使用 Compose，或使用后面的原生程序。

```bash
mkdir -p data
docker run -d --name wb2api \
  --restart unless-stopped \
  -p 7863:7863 \
  -e TZ=Asia/Shanghai \
  -e WB2A_AUTH_DIR=/app/data/auths \
  -e WB2A_STATE_FILE=/app/data/state.json \
  -v "$PWD/data:/app/data" \
  ghcr.io/csszyft/wb2api:v1.9.45
```

此方式与 Compose 使用相同的数据目录和端口。容器首次以 root 进入入口脚本修正数据卷权限，再降权为 UID `10001` 运行服务。

### 方式三：下载 Windows / Linux / macOS 程序包

从 [本仓库 Releases](https://github.com/csszyft/wb2api/releases/latest) 下载对应平台的压缩包并完整解压：

| 系统 | 文件名中的平台 |
|---|---|
| Windows 64 位 | `windows_amd64.zip` |
| Linux x86_64 | `linux_amd64.tar.gz` |
| Linux ARM64 | `linux_arm64.tar.gz` |
| macOS Intel | `darwin_amd64.tar.gz` |
| macOS Apple Silicon | `darwin_arm64.tar.gz` |

Windows 在解压目录打开 PowerShell：

```powershell
.\wb2api.exe -config config.json
```

Linux / macOS 在解压目录运行：

```bash
./wb2api -config config.json
```

首次启动缺少配置文件时会自动生成，密钥在同目录的 `config.json` 中。若自行复制了 `config.example.json`，请把其中的 `test_key` 改成自己的密钥。

Windows 程序包还提供 `start-wb2api.cmd`、`stop-wb2api.cmd`、`status-wb2api.cmd`，用于后台启动、停止和查看状态。三件套管理由启动脚本创建的实例；前台运行的程序用 `Ctrl+C` 停止。

### 方式四：从源码构建

安装 Go，版本至少满足 [go.mod](go.mod) 的要求，然后执行：

```bash
git clone https://github.com/csszyft/wb2api.git
cd wb2api
go build -trimpath -ldflags "-s -w" -o wb2api ./cmd/server
./wb2api -config config.json
```

Windows 将输出名改为 `wb2api.exe`，使用 `.\wb2api.exe -config config.json` 启动。前端资源随 Go 程序内嵌，无需安装 Node.js 构建页面。

## 添加账号与客户端接入

1. 打开 `/panel/`，填写配置中的 API Key。
2. 点击“添加账号”，选择国内或国际区域，打开授权链接并完成登录；也可以切换到“导入 JSON”选择凭证文件。
3. 看到导入成功后，账号已经进入池中。激活、签到与余额在后台更新，无需重启。
4. 在“模型与档位”查看所需区域的模型，或通过 `/v1/models` 获取模型列表。
5. 客户端选择 OpenAI 兼容接口，Base URL 填 `http://服务器地址:7863/v1`，Key 填 `api_key`，模型名使用列表中的 ID。

凭证需要 UID、Access Token 和 Refresh Token 等必要数据；无法识别或缺字段的条目会显示具体原因。国际凭证例如 `"domain":"www.workbuddy.ai","realm":"intl"` 可自动识别，无需手改为 `global`。

### 验证接口

以下示例使用 Bash；将 `YOUR_API_KEY` 和模型 ID 替换为实际值。示例 DeepSeek 模型需要国际服账号及对应模型可用。

```bash
curl -s http://127.0.0.1:7863/healthz

curl -s http://127.0.0.1:7863/v1/models \
  -H 'Authorization: Bearer YOUR_API_KEY'

curl -sN http://127.0.0.1:7863/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

非流式请求将 `stream` 改为 `false`。没有可用账号时，`/healthz` 可返回 `503`；这时先检查账号池，不能仅凭这个状态判断进程没有启动。

## 代理池与账号固定出口

`v1.9.45` 新增“出口代理”页面。支持 HTTP、HTTPS、SOCKS5 和 SOCKS5h，账号的聊天、刷新凭证、余额、模型查询及任务请求统一走绑定代理。SOCKS5h 由代理端解析目标域名。

1. 打开“出口代理”，设置每个 IP 允许绑定的 WorkBuddy 账号数，默认 `1`。
2. 粘贴代理链接，点击“导入并检测出口”。默认勾选“检测完成后分配未绑定账号”，已有绑定保持不变。
3. 检测完成后，出口列表显示实际公网 IP、容量和已绑定账号数。两条链接若检测出同一个 IP，共用该 IP 的容量。
4. 勾选“启用账号代理”并保存。新账号自动分配可单独开启；也可按国内／国际区域一键分配，或逐账号选定出口。

完整链接示例（使用你自己的主机、端口、用户名和密码）：

```text
http://分组.代理账号:密码@主机:端口
socks5h://分组.代理账号:密码@主机:端口
```

`sg`、`us` 等仅为代理商定义的国家或分组用户名，程序保留你的输入。“代理账号”指完整代理用户名中的固定出口标识，和 WorkBuddy 账号分别管理。从基础链接生成时，程序在用户名后附加 `.代理账号`；其他供应商格式可用 `{account}` 指定位置，例如 `socks5h://分组.{account}:密码@主机:端口`。生成结果保存后固定，重启不会重新生成。

批量导入兼容 `用户名:密码@主机:端口`、`主机:端口:用户名:密码`、`用户名:密码:主机:端口`、竖线／逗号分隔及 JSON 的 `host/server`、`port/server_port`、`username/user`、`password/pass` 字段。没有协议的行使用页面选定的类型；无法确定字段顺序时提示改用完整 URL。特殊字符可用 URL 编码，如密码中的 `@` 写为 `%40`。

代理池未启用时使用原出口。启用后，默认未绑定账号等待分配；如需继续用原出口，可打开对应开关或逐账号明确选择。绑定代理失败会单独报告代理错误，避免将健康账号计为故障；WAF 多账号拦截按实际出口 IP 隔离。代理出口故障不作为免费额度耗尽，也不触发 SG／国内付费回退。

代理与账号绑定保存在状态文件同目录的 `proxy-pool.json`，常见位置为 `data/proxy-pool.json`。该文件含代理认证信息，迁移时和现有数据目录一起保留。面板列表隐藏代理密码。程序检测出口 IP 后保存观测值，实际固定出口由代理商保证；代理商变更出口后可再次检测并调整绑定。

## 路由与额度设置

配置页可编辑大部分运行参数，完整字段参考 [config.example.json](config.example.json)。以下为相关配置片段，合并进已有配置即可：

```json
{
  "pool": {
    "pick_mode": "sequential",
    "reserve_credits": 50,
    "deepseek_sg_fallback": true
  },
  "models": {
    "strip_realm_prefix": true,
    "realm_precedence": "global"
  }
}
```

| 设置 | 说明 |
|---|---|
| `pool.pick_mode` | `sequential` 为 Fill First，`weighted` 为加权选号；默认 `weighted` |
| `pool.reserve_credits` | 保留积分线，默认 `50`；余额触底时按模型免费／低价判定限制消费 |
| `pool.deepseek_sg_fallback` | 默认开启；国际免费 DeepSeek 全池耗尽后转 SG，SG 全池额度耗尽或已知余额达到保留线后，无前缀请求可继续转国内同名 4.1。国内按积分计费，需已知余额高于保留线；网络、WAF、忙碌、缺模型或余额未知不作为额度耗尽 |
| `models.strip_realm_prefix` | 默认 `true`，对外模型列表使用裸名 |
| `models.realm_precedence` | 默认 `global`，也可设 `cn`；应用于两域共有或归属未知的裸名模型 |
| `global.enabled` | 国际区域路由开关，关闭时保留原有国内锁定行为 |

模型分组来自最近成功拉取的目录，启动预热、模型列表和面板查询会更新分组；暂时拉取失败保留已有记录。聊天路由读取本地快照，不为每次请求额外查询上游。上游之后给另一地区新增模型支持，成功刷新目录后即可参与对应区域路由。

每日 Token 限额在账号行“模型用量 / 限额”设置，按**每个账号、每个模型**分别保存。`100M` 是 `100,000,000` Token，默认不限。达到上限后停止向该账号分配此模型的新请求；正在进行的请求会完成，因此最终用量可能略超限，未收到上游 usage 的请求也无法精确计量。

## 面板与运维

| 页面 | 用途 |
|---|---|
| 账号池 | 国内／国际账号状态、拖动排序、余额、签到、启停、测试对话、每日模型限额、凭证导出 |
| 用量 | 按账号、模型、区域和时间范围查看用量 |
| 积分构成 | 积分包剩余量、到期批次与到期提醒 |
| 模型与档位 | 分区模型目录、倍率、思考档位、上下文与输出上限 |
| 出口代理 | 批量导入代理、检测公网 IP、生成代理账号、设置每 IP 容量与固定账号绑定 |
| 任务中心 | 查看上游提供的成长任务与执行队列；活动与奖励以官方当期规则为准 |
| 配置 | 在线保存配置；需要重启的字段会显示提示 |
| 运行日志 | 请求记录、首字延迟、耗时、Token、错误结果，以及任务／系统日志 |

程序支持定时签到、活跃上报、旅行、Token 保活和余额刷新。国内与国际的接口与任务能力存在差异；已结束的活动或变更后的奖励以官方状态为准。

### 数据目录与备份

Compose 和上面的 Docker 命令只需持久化一个目录：

| 宿主机路径 | 容器路径 | 内容 |
|---|---|---|
| `./data/config.json` | `/app/data/config.json` | 配置和网关 API Key |
| `./data/auths/` | `/app/data/auths/` | 账号登录凭证 |
| `./data/state.json` | `/app/data/state.json` | 账号池状态、顺序、每日模型限额与计数 |
| `./data/usage.json` | `/app/data/usage.json` | 用量记录 |
| `./data/request-logs/` | `/app/data/request-logs/` | 启用归档时的请求元数据 |

原生程序默认凭证目录为 `./auths/`、状态与用量位于 `./data/`、配置为 `./config.json`。完整迁移应同时备份配置、凭证和数据目录。面板“导出凭证”只导出登录凭证，不包含每日限额及历史用量。

升级前建议停止服务后备份数据，升级时保留原数据目录。容器升级用 `docker compose pull` 和 `docker compose up -d`；原生程序停止后替换程序包，再启动。

### 常见问题

**导入成功后余额还没显示？** 凭证入池成功与余额刷新分开执行，稍后刷新面板。持续没有余额时检查运行日志中的后台激活、签到或余额查询错误。

**模型列表加载失败？** 点击“重新获取”，并确认对应区域有可用账号。测试弹窗可先使用默认模型，但实际是否可调用仍取决于账号及上游支持。

**国内独有模型仍被发到国际？** 确认运行版本至少为 `v1.9.40`，刷新对应区域目录；显式填写了 `global:` 前缀时仍按该前缀路由。

**国际 4.1 和 SG 都用完了，国内同名模型仍没被使用？** 升级至 `v1.9.41`，使用无前缀 `deepseek-v4.1-flash`，保持 SG 回退开启，并确认国内账号余额已刷新且高于保留积分线。显式 `global:` 仍固定在国际区；直接请求 `deepseek-v4.1-flash-sg` 也不会改成国内 4.1。新请求在国际免费额度或 SG 恢复后重新优先它们。

**多图会话收到 413？** `server.max_body_mb` 默认 `0`，表示关闭网关请求体上限。显式设为正数后，超过该大小会返回 `413 request_body_too_large`；可在配置页调大或置 `0`，保存即时生效。例如配置 `32` 时的响应如下：

```json
{"error":{"message":"请求体超过网关内存护栏 32 MB：这是网关侧上限（防止单请求吃爆进程内存），不是上游限制；网关默认不设该上限（server.max_body_mb=0 = 不预拦截，大请求交上游自然响应），当前值由运维显式配置——可在面板「请求体上限」调大或置 0 关闭（保存即时生效）。多图会话易触发：历史图片每轮以 base64 重发（膨胀约 37%），请压缩图片或调大上限后重试","type":"api_error","code":"request_body_too_large"}}
```

**流中断或 WAF 错误？** HTTP `200` 只说明流已建立，不代表完整生成成功。请提供发生时间、版本、模型及对应运行日志；已经输出内容的流不能简单靠整段重试无缝续接。

**配置保存提示 permission denied？** 当前镜像的配置应位于可写的 `/app/data/config.json`。检查数据卷和 UID `10001` 的写入权限；不要沿用旧版只读挂载 `/app/config.json` 的方式。

## 开发与发布

```bash
go test ./...
go vet ./...
```

发布流程分为 [程序包构建](https://github.com/csszyft/wb2api/actions/workflows/release.yml) 与 [容器镜像构建](https://github.com/csszyft/wb2api/actions/workflows/docker.yml)。正式版本由 Git tag 注入，镜像发布 `v版本号` 与 `latest`，支持 `linux/amd64` 和 `linux/arm64`。程序包支持 Windows、Linux 和 macOS，具体资产见 Releases。

反馈问题请前往 [本仓库 Issues](https://github.com/csszyft/wb2api/issues)，附运行版本、部署方式、发生时间、模型、脱敏日志及复现步骤。不要上传 API Key、Access Token、Refresh Token 或完整凭证。

## 来源与许可

- 本仓库：[csszyft/wb2api](https://github.com/csszyft/wb2api)
- 面板上游：[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
- 原始项目：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)

采用 [MIT License](LICENSE)，保留原作者版权与许可声明。本项目为非官方网关，请仅使用本人或获授权的账号，并遵守相关服务条款。服务默认提供 HTTP；公网使用时应配置鉴权与 HTTPS 反向代理。配置文件与导出凭证包含敏感数据，请妥善保存。
