# AgentDeck 介绍片

83.5 秒、1920×1080 @30fps、中文旁白字幕 + 英文对照的介绍片，产出
`out/final.mp4`。

做法照 [SenseNova-6.8-Code2Video](https://github.com/OpenSenseNova/SenseNova-6.8-Code2Video)
的路子：**用代码做视频**，每一帧都是 `t` 的纯函数，无头浏览器逐帧截图、
ffmpeg 合成，而不是录屏或采样生成画面。好处是同一份代码永远渲染出同一支片子，
改一句台词只重渲，字幕和镜头推拉按秒对齐，不用事后在剪辑软件里对波形。

## 跑一遍

```bash
cd video
npm install            # 只装 playwright（浏览器用本机 Chrome）
./tools/build.sh       # 全流程，约 13 分钟
```

`build.sh` 依次做六件事，全部本地、不联网（连模型都不跑，见下面那节）：

1. `tools/mk-demo-project.sh` — 造一个**仓库外**的演示项目
   （默认 `/home/soar/Projects/agentdeck-demo`），带真实的未提交改动，
   给「工作区面板」那一镜当素材。放在仓库外是因为面板要渲染 `git diff`，
   在 cc-webui 自己的 worktree 里造脏文件会污染最后的合并。
2. `go build` + `tools/seed-demo.py` — 生成演示数据集。
3. 起演示服务：`./cc-webui -config video/demo-config.yaml`，
   **127.0.0.1:8323 + 独立 data_dir（`data-video/`）**。
4. `tools/capture.mjs` — Playwright 登进去把各视图截成 `assets/*.png`。
5. `tools/render.mjs` — 逐帧 seek + 截图，PNG/JPEG 直接灌进 ffmpeg 管道
   （不落盘，所以 4K 超采样也不占磁盘）。
6. `tools/music.py` — 合成配乐；`ffmpeg` 混流成 `out/final.mp4`。

## 为什么演示数据是预置的

两个理由，都不是为了省事：

- **隐私**：:8321 上那个正在 dogfood 的实例里是真会话，截图不能带出来。
  所以演示实例跑在自己的端口和 data_dir 上，内容由 `seed-demo.py` 写死。
- **可复现**：片子要每次渲染都一模一样，所以会话正文、工具卡、diff 全钉在
  `seed-demo.py` 里。写的是 canonical 事件形状（见
  `internal/claudehist.ConvertIn` 的生产侧），前端渲染的是**真实组件**，
  只是内容是造出来的。

演示会话的 `claude_session_id` 一律留空，这是刻意的：`handleEvents` 只在
没有外部 transcript 可合并时才逐条回放存储事件——正好是 fixture 想要的。

统计弹窗那一镜的 `/api/stats` 由 Playwright 拦截，喂一份合成数据：
既不用等 25 秒冷扫，也不会把本机的真实统计数字带进片子里。

## ⚠️ 演示配置里的 CLI 路径是故意写错的

`demo-config.yaml` 里五个 cli 的 `path` 全指向 `/nonexistent/agentdeck-demo/*`，
**别改回真实路径**。

种子数据里有几个会话标了 `resident: true`（为了让 📡 常驻标签、地址名、常驻
开关在界面上真实可见），而服务启动时的 `mgr.RestoreResidents()` 会为这些会话
真的 spawn 一个 `claude --input-format stream-json` 常驻进程——provider 的 env
为空时端点由 CLI 自己的 `~/.claude/settings.json` 给，也就是说**它会带着你的
真实凭据起来**。这个坑踩过一次：残留了一个真的 claude 常驻进程，还往已被删掉的
worktree 路径写 settings 文件。

指向不存在的路径后 spawn 立刻失败，什么都不会发生；片子需要的一切都来自 store
里的会话字段，不依赖进程活着。

## 渲染为什么分片并行

逐帧渲染卡在浏览器把一帧 2880×1620 的截图编码出来（~280ms/帧），而每个页面只用
一个线程——所以唯一的杠杆是同时开几个浏览器。`build.sh` 默认 `CHUNKS=3`，把片子
切成 3 段各起一个 Chrome 并行渲，再用 concat `-c copy` 拼起来（`render.mjs` 给每片
第一帧强制 IDR，否则拼接点会从 GOP 中间开始、整段花掉）。实测 11.7 分钟 →
5.2 分钟（4 核机器）。

```bash
CHUNKS=1 ./tools/build.sh                      # 退回串行
CHUNKS=3 ./tools/render-parallel.sh out/x.mp4  # 单独用并行渲染器
node tools/render.mjs --chunk 1/3 -o out/p1.mp4  # 手动渲某一片
```

## 时间表只有一份

`src/timeline.js` 不碰 DOM，装着场景起点（`SC`）和每个场景的**具名节拍**（`B`）。
`src/film.js` 读这些名字驱动动画，不再各处散落魔数——**改时间只动这一个文件**。
上一轮把散落的数字抽进来时先存了 9 张参考静帧，重构后逐字节比对，确认画面没变。

## 只改音乐不用重渲视频

```bash
./tools/audio-only.sh          # 重生成配乐 + 换音轨，几秒钟
```

视频流直接 `copy`，只有音轨重编码。配乐是 122 BPM 的四踩底鼓 + 反拍军鼓 +
八分贝斯 + 拨弦琶音（D 大调 I–V–vi–IV），带侧链；标题段和收尾段鼓退出。
**成片里只有音乐床、没有音效**——试过一版逐节拍的音效，听下来太碎，整层撤掉了。
细节见 `tools/music.py` 顶部注释。

## 动画的两条规矩

1. **不做无意义的推镜**。给截图加缓慢推近（Ken Burns）想「别太静」，结果被指
   「还不如没有」。镜头只在有理由时动：跟着字幕指的功能推、给机构图让位、为了
   看清 diff 推近。
2. **画面里的运动要表达内容**。agent2agent 那镜的运动全是「消息在两端之间走」：
   顶部一条常驻的会话连线，发件卡亮一道扫光，一枚光点从卡片飞到对端 chip，
   落地闪一下，切视角后光点落进对话流、入站气泡滑入落位。提问那镜同理：
   提问卡从 composer 下方升起，通知横幅从上方落进来。

## 要动画的东西要单独抠出来

「消息抵达」「通知落下」这两个动画没法靠一张整屏截图做出来——截图里的气泡和
横幅是画死的。做法是：先把那个元素 `visibility:hidden` / 清空，截一张**没有它**
的整屏帧，再把那个元素单独裁剪成一张小图（`shotEl`，带 14px 内边距容阴影），
片子按量好的矩形把它滑入 / 落下。`assets/rects.json` 里记着这些裁剪框的坐标。

## 截图里的坐标是量出来的

`tools/capture.mjs` 在每次截图前会把关键元素的矩形写进 `assets/rects.json`
（asset 像素）。`src/film.js` 里的镜头推拉 `zoom/cx/cy` 和聚光灯矩形全部照它填。
肉眼估过一次，坐标差了一倍，白烧了一轮 11 分钟的渲染。

## 目录

| 路径 | 说明 |
|---|---|
| `SPEC.md` | 镜头表、字幕文案、素材清单 |
| `VERIFY.md` | 交付前的核对记录（改完片子照着过一遍） |
| `demo-config.yaml` | 演示实例的隔离配置（独立端口 + data_dir） |
| `src/film.html` / `src/film.js` | 片子本体：`window.__seek(t)` 渲染任意一帧 |
| `tools/seed-demo.py` | 造演示数据集（`data-video/`） |
| `tools/mk-demo-project.sh` | 造仓库外的演示项目 |
| `tools/capture.mjs` | Playwright 截图 |
| `tools/render.mjs` | 逐帧渲染 → ffmpeg |
| `tools/music.py` | 纯标准库合成配乐 |
| `tools/build.sh` | 全流程 |
| `tools/contact-sheet.sh` | 审片用联系表 |
| `tools/text-check.mjs` | 安全区/字号/文字重叠审计 |
| `tools/render-parallel.sh` | 分片并行渲染 + 无损拼接 |
| `assets/` `out/` | 产物，已 gitignore |

## 单独重渲

```bash
node tools/render.mjs --still 33          # 看某一帧（改片子时最常用）
node tools/render.mjs --stills 3,9.5,20   # 一批帧，一次开浏览器
node tools/render.mjs --range 25:40       # 只渲一段，迭代时省时间
node tools/render.mjs                     # 全片（默认 dsf=1.5，约 11 分钟）
node tools/render.mjs --dsf 2 --crf 14    # 更高质量、更慢
```

`--dsf 2` 时画布 backing store 是 3840×2160，再由 ffmpeg 用 lanczos 降到
1080p，中文字形更干净；`dsf=1.5` 是画质与时间的折中。中间帧用 JPEG（q97）
而不是 PNG——浏览器编码一张 4K PNG 要 ~1.5s，是整条流水线的瓶颈，而
x264 反正会重编码一次。

## 片子怎么组织的

- `src/film.js` 里 **没有** `requestAnimationFrame`、**没有** CSS transition，
  只有 `draw(t)`。任何跟时间有关的东西都必须从 `t` 推导，或者从固定种子的
  PRNG（`mulberry(20261030)`）来，否则粒子场每渲一次就抖一次。
- 镜头表在文件末尾的 `SCENES`：9 个场景，每个是
  `{id, label, dur, draw(p, local)}`。场景切换是**交叉溶解**——驱动层先把
  上一个场景画完，再把下一个画在上面。前提是**没有任何场景铺满全屏的不透明
  背景**（只有卡片、手机壳、截图），所以不需要离屏合成；新加场景时别破坏
  这条，否则交叉溶解会变成黑场硬切。
- 截图里的「聚光灯」注释（`spotlight`）用的是**源图像素坐标**，经 `shot()`
  返回的映射函数换算到屏幕，所以镜头推拉时注释会跟着走。
- 配色直接用 AgentDeck 自己的深色主题色值，片子和产品看起来是一套东西。
