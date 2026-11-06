# AgentDeck 内网使用 ducc / ducx

> 目标读者：第一次在内网机器上启动 AgentDeck，并用 ducc / ducx 开始对话的人。
> 完整功能见 [README.md](../README.md)，开发与架构说明见 [AGENTS.md](../AGENTS.md)。

## 最快上手

```bash
cp du_int.config.example.yaml config.yaml
# 编辑 config.yaml：改 gateway_token，并把 ducc / ducx 的 path 改成实际路径
go build -o cc-webui .
./cc-webui
```

浏览器打开 `http://<机器IP>:8321`，输入 `gateway_token` 登录；新建会话时 CLI 选 `ducc` 或 `ducx` 即可。

## 1. 前置条件

- 一台 Linux / macOS 内网机器，能访问模型网关。
- ducc 和/或 ducx 已安装，并且已经用**启动 AgentDeck 的同一个系统用户**在终端完成登录。AgentDeck 不管理它们的账号密码，只使用它们自己的登录态。
- 自检：下面两条命令能列出模型，说明登录态正常。

  ```bash
  ducc models      # 应输出 Available Models: ...
  ducx models      # 应输出模型清单
  ```

- 自己构建需要 Go 1.22+，并且能获取 Go 模块依赖；否则直接使用现成的 `cc-webui` 二进制。

## 2. 配置：复制示例，只改这些

```bash
cp du_int.config.example.yaml config.yaml
```

`config.yaml` 已被 gitignore，不会进仓库。按下表改：

| 字段 | 说明 |
|---|---|
| `gateway_token` | Web 登录口令。换成一串足够长的随机值（例如 `openssl rand -hex 24` 的输出）。**这不是 ducc / ducx 的密码。** 留空则启动时随机生成并打印到控制台；也可用 `GATEWAY_TOKEN` 环境变量覆盖。 |
| `listen` | 监听地址。默认 `0.0.0.0:8321`；只给本机用可改 `127.0.0.1:8321`。 |
| `data_dir` | 会话元数据与事件流目录。建议写绝对路径；默认 `./data` 是相对**启动目录**，不是相对 config 文件。 |
| `default_cwd` | 新建会话时「工作目录」输入框的默认提示。可写 `~` 或绝对路径。 |
| `clis` 里 ducc 的 `path` | 填 `command -v ducc` 的输出（绝对路径）。官方安装器常见位置：`~/.baidu-cc/baidu-cc/bin/ducc`。 |
| ducc 的 `permission_mode` | `default`：工具调用在 Web 里弹审批卡；`bypassPermissions`：全部放行。建议先用 `default`。 |
| `clis` 里 ducx 的 `path` | 填 `command -v ducx` 的输出。官方安装器常见位置：`~/.baidu-cx/baidu-cx/bin/ducx`。 |
| ducx 的 `home` | ducx 的数据目录，默认 `~/.baidu-cx`；没改过安装位置就保持不动。 |
| ducx 的 `model_provider` | 保持 `oneapi`，**不要删**（原因见「常见坑」）。 |
| ducx 的 `permission_mode` | 示例里给的是 `bypassPermissions`（完全放开）。想更稳妥就改成 `default`：workspace-write 沙箱，工作区内可读写、不弹审批卡。 |

`providers` 和 `models` 一般不用改：模型清单由 CLI 自己提供，写死在 config 里会过期。

**`path` 里的 `~` 不会展开**，必须写绝对路径，或写一个能在 `PATH` 里找到的命令名。

## 3. 构建与启动

```bash
go build -o cc-webui .
./cc-webui

# 指定配置文件 / 覆盖端口
./cc-webui -config /path/to/config.yaml -listen 0.0.0.0:8321

# 环境变量覆盖
GATEWAY_TOKEN='<token>' CC_WEBUI_LISTEN=0.0.0.0:8321 CC_WEBUI_DATA_DIR=/path/to/data ./cc-webui
```

用 screen / tmux / systemd 等方式保持进程常驻。启动失败时看终端日志：配置解析、端口占用、CLI 路径都会直接报出来。

## 4. Web 登录

1. 浏览器打开 `http://<机器IP>:8321`。
2. 输入 `gateway_token`，点「登录」。登录态保存在浏览器 cookie `ccgw_token` 里。

- 在 `config.yaml` 里改 token：3 秒内热加载生效，已有登录失效，刷新页面用新 token 重新登录。
- 用 `GATEWAY_TOKEN` 环境变量改 token：需要重启进程。

## 5. 新建会话：CLI / Provider / Model 怎么选

点「新建会话」，填一个**存在的工作目录**，然后选：

| 想用的 CLI | CLI 下拉 | Provider 下拉 |
|---|---|---|
| ducc | `ducc` | `ducc-models` |
| ducx | `ducx · codex` | `codex-ducx` |

- CLI 与 Provider 必须属于同一引擎；正常下拉会自动过滤，配错后端会返回 `engine mismatch`。
- **Model** 下拉在打开新建会话时从 CLI 读取（ducc 走 `ducc models`，ducx 读自己的模型目录）。选一个即可，不要在 config 里维护模型列表。
- **权限模式**先用默认；需要 Agent 连续执行、不想逐次点审批时再换成 bypass。
- 会话在创建时绑定引擎，**不能跨引擎切换**。同一引擎内可以在输入框上方热切换 CLI / Provider / Model / 权限，上下文不丢。

## 6. 模型与权限注意点

- **ducc**：provider `ducc-models` 的 env 为空，端点和凭据由 ducc launcher 自己注入。模型清单随账号和 CLI 版本变化，所以 config 里 `models: []` 是对的。
- **ducx**：模型和可用推理档位来自 `<home>/model-catalogs/oneapi-models.json`（`ducx models` 打印的就是它）。不同模型的档位不一样，下拉里没有的档位就是该模型不支持。
- **ducx 的 `model_provider` 不能删**：只要 AgentDeck 给 ducx 传 `-c`（每轮的 HTML 渲染约定、推理档位都会传），ducx 就不再自动注入自己的 provider。保留 `model_provider: "oneapi"` 才会继续走内网网关；删掉或写错会退回 `api.openai.com`，表现为连不上或请求失败。
- **ducc 权限**：`default` 下每次工具调用在 Web 里弹审批卡（允许本次 / 本会话同名工具 / 拒绝），55 秒不回答自动拒绝。`bypassPermissions` 不弹卡、全部放行。
- **ducx 权限**：`default` = `--sandbox workspace-write`，工作区内可读写、不弹卡（headless 没有审批面）；`plan` / `readOnly` = 只读沙箱；`bypassPermissions` = 无沙箱、无审批，权限最大。
- 建议两个 CLI 都先用默认权限，确有必要再开 bypass。改权限/模型在下一轮生效，不需要新建会话。

## 7. 常见坑

1. **会话起不来 / CLI 路径错**：`path` 写绝对路径（`command -v ducc`、`command -v ducx`）；`~` 在 `path` 里不展开。`data_dir` 相对启动目录，换目录启动会找不到旧数据。
2. **模型下拉为空，或建会话提示「请先选择配置」**：先在终端跑 `ducc models` / `ducx models`，确认登录态和模型目录正常。前端会把结果（包括失败）缓存 10 分钟，修好后**刷新页面**。
3. **ducx 请求失败 / 连到官方端点**：检查 `clis` 里 ducx 的 `model_provider` 是否仍为 `oneapi`。
4. **改了 token 登录不上**：旧 cookie 已失效，用新 token 重新登录；用环境变量改 token 的要重启进程。
5. **改了 `listen` / `data_dir` 不生效**：这两个字段在启动时固化，必须重启；其他字段（token、clis、providers、models）3 秒热加载。
6. **工作目录报错**：创建会话要求 cwd 存在且是目录，先 `mkdir -p` 或改对路径。`default_cwd` 只是输入框的默认提示，提交前确认实际路径。
7. **用错系统用户启动**：必须用登录过 ducc / ducx 的同一用户启动；换 root / systemd 的其他用户会找不到登录态和历史。
8. **端口访问不到**：检查 `listen`、防火墙 / 安全组、访问用的 IP。只给本机用就绑 `127.0.0.1`。
9. **不要直接暴露公网**：`gateway_token` 是唯一鉴权，建议只在内网 / VPN 使用，或在前面加反向代理。

---

相关：[README.md](../README.md) · [du_int.config.example.yaml](../du_int.config.example.yaml) · [AGENTS.md](../AGENTS.md)
