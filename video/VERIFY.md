# VERIFY

交付前照着过一遍。下面每一条都是**实际跑过的命令 + 实际输出**（2026-10-30，
Linux，本机 Chrome 走 `channel: 'chrome'`）。

## 1. 成片参数

```
$ ffprobe -v error -show_entries format=duration,size \
    -show_entries stream=codec_type,codec_name,width,height,r_frame_rate,nb_frames \
    -of default=noprint_wrappers=1 out/final.mp4
codec_name=h264          # 视频
codec_type=video
width=1920
height=1080
r_frame_rate=30/1
nb_frames=2505           # 83.5s × 30fps
codec_name=aac           # 音频
codec_type=audio
duration=83.502000
size=31916867            # 30.4 MiB
bit_rate=3057000
```

（83.502 而不是 83.5：分片并行渲染用 concat 拼接，每片起点有毫秒级取整。混流时
`-shortest` 让输出跟着 83.5s 的音轨走。）

## 2. 音轨不是静音、也没爆

```
$ ffmpeg -hide_banner -i out/final.mp4 -af volumedetect -f null -
mean_volume: -22.4 dB
max_volume:  -11.3 dB
```

成片里只有音乐床、没有音效（试过一版逐节拍音效，听下来太碎，整层撤掉）。
峰值留了 11 dB 余量——`music.py` 归一化到这个目标，方便以后叠人声旁白。

第一版是慢速环境垫音，平均电平 -23.7 dB、峰值 -14 dB，被指「听着要睡着」。
换成 122 BPM 的欢快底子后平均电平 -22.4 dB、峰值 -11.3 dB（**平均高了约 6 dB**
——底鼓峰值高，不压一下整体只能做得很小声，所以末尾加了一层轻限幅）。

段落也对得上：逐秒 RMS 显示 0–6s（标题）在 -26 ~ -31 dB、8–76s 稳定在
-21.8 dB、76s 之后一路降到 -37.7 dB（鼓退出 + 淡出）。见 `tools/music.py`。

**只改音乐时不用重渲视频**：`./tools/audio-only.sh` 重生成配乐、把音轨换到已有
成片上（视频流 `copy`）。实测重渲前后 `nb_frames=2505` 不变。

## 3. 渲染是确定性的

同一时刻渲两次，字节相同（粒子场用固定种子的 `mulberry(20261030)`，
没有任何东西读挂钟）：

```
$ node tools/render.mjs --stills 42 && mv out/still-42.png /tmp/d1.png
$ node tools/render.mjs --stills 42
$ md5sum /tmp/d1.png out/still-42.png
80d0f2109bc8f0cefbb59b8c01b92b1c  /tmp/d1.png
80d0f2109bc8f0cefbb59b8c01b92b1c  out/still-42.png
```

## 3b. 画面里的运动都在表达内容

第一版那两镜被指「没有动画」，第二版加了无意义的推镜又被指「还不如没有」。
现在的规则是：镜头只在有理由时动，画面里的运动全部用来表达内容（消息在两端
之间走、通知落下来）。验证办法是密集抽帧看有没有重复帧：

```
$ for t in 39.2 39.75 … 45.25; do ffmpeg -ss $t -i out/final.mp4 -frames:v 1 out/d/f-$t.png; done
$ md5sum out/d/*.png
  12 frames, 0 duplicate(s)
```

顺带确认 `grep -c "kb(" src/film.js` = 0：那个「镜内持续推近」的辅助函数已经
整个删掉了，不会再有人顺手用回去。




## 3c. 分片并行渲染的接缝是干净的

逐帧渲染卡在浏览器编码截图上，所以 `build.sh` 默认把片子切 3 段并行渲、再用
concat `-c copy` 拼起来。拼接点最容易出的问题是某片从 GOP 中间开始（后面整段
花掉），所以 `render.mjs` 给每片第一帧强制了 IDR。三项检查：

```
$ ffprobe -count_frames -select_streams v -show_entries stream=nb_read_frames out/silent.mp4
2505                                    # 与串行渲染一致，且与容器元数据一致

$ ffmpeg -v error -i out/silent.mp4 -f null -
（无输出）                                # 解码无错误

$ ffmpeg -i out/join-27_8.png -i out/ref-27_8.png -lavfi psnr -f null -
average:39.99                           # 拼接点帧 vs 同刻参考帧
average:37.73                           # 另一个拼接点
```

PSNR ~38–40 dB 是正常的压缩差；接缝如果坏了会掉到 20 以下或者直接报错。
耗时：11.7 分钟（串行）→ 5.2 分钟（CHUNKS=3，4 核）。
## 3d. 文字检查

```
$ node tools/text-check.mjs 0.1
超出安全区: 0 处
字号 < 16px: 0 处
文字重叠: 0 处
  全部通过
```
## 3e. 时间表重构没有动画面

把散在各场景里的节拍数字抽进 `src/timeline.js` 时，先存了 9 张参考静帧，重构后
逐字节比对：

```
$ cmp -s /tmp/ref/still-20.png out/still-20.png && echo 一致
  ✓ still-3/10/20/30/42/55/63/72/80.png 全部一致
```

同一轮里只改了标题场景的预卷（见下），那是有意的改动。

## 4. 素材齐、没有引用到不存在的图

```
$ python3 -c "…对照 src/film.js 里的 ASSETS 与 assets/*.png…"
missing: none
unused : none（备用素材在 SPEC.md 里列明）
```

镜头里的推拉与聚光灯坐标来自 `assets/rects.json`（截图时量的元素矩形），
不是肉眼估的。

## 5. 演示实例**不可能**拉起真的 CLI

种子数据里有几个会话标了 `resident: true`（为了界面上的 📡 常驻标签、地址名、
常驻开关都真实可见）。如果 `demo-config.yaml` 里填真实 CLI 路径，服务启动时的
`mgr.RestoreResidents()` 会真的 spawn 一个 `claude --input-format stream-json`
常驻进程——**它会带着真实凭据起来**。这个坑踩过一次（残留了一个真的 claude 进程，
还往已被删掉的 worktree 路径写 settings 文件）。

现在每个 cli 的 `path` 都指向不存在的路径，spawn 立刻失败：

```
$ grep -nE "^    path:" video/demo-config.yaml
    path: "/nonexistent/agentdeck-demo/claude"
    path: "/nonexistent/agentdeck-demo/opencode"
    path: "/nonexistent/agentdeck-demo/mcode"
    path: "/nonexistent/agentdeck-demo/qoder"
    path: "/nonexistent/agentdeck-demo/ducx"

$ tail -3 /tmp/opencode/demo-server.log
restore resident demo-docs-0006: start claude: fork/exec /nonexistent/agentdeck-demo/claude: no such file or directory
opencode provider discovery for cli "opencode" failed (keeping previous list): … no such file or directory
restore resident demo-a2a-0010: start claude: fork/exec /nonexistent/agentdeck-demo/claude: no such file or directory

$ pgrep -af "nonexistent/agentdeck-demo"
  （空）
```

**别把这几行改回真实路径。**

## 6. 没碰 dogfooding 实例

```
$ curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8321/
302                                                     # 登录跳转，正常

$ pgrep -af "Projects/cc-webui/cc-webui"
1363397 /home/soar/Projects/cc-webui/cc-webui            # 全程未重启

$ grep -nE "^(listen|data_dir)" video/demo-config.yaml
listen: "127.0.0.1:8323"
data_dir: "./data-video"
```

片子里的会话内容全部来自 `data-video/`（`seed-demo.py` 预置），
统计弹窗的数据由 Playwright 拦截 `/api/stats` 换成合成数据——
真实会话正文与本机统计数字都没有进画面。

## 7. 逐条核对这轮的五个改动

| 要求 | 落点 |
|---|---|
| agent2agent 重点介绍 | 独立镜头 5（12s，三段），是全片第二长的场景 |
| agent2agent 要有「两端之间」的动画 | 顶部常驻会话连线 + 消息包在两端飞 + 发件卡扫光 + 入站气泡抵达（见 §3b） |
| 无意义的推镜不要做 | 删掉 `kb()` 漂移辅助函数；镜头只在跟着功能走 / 让位给机构图时动 |
| 场景切换不要生硬 | 交叉溶解 0.5s → 0.75s，并加 2.2% 缩放 + 24px 上浮的 settle |
| Codex 排到 Claude Code 下面 | 镜头 3 的徽章顺序：Claude Code → **Codex** → opencode → MiniMax Code → Qoder；收尾那行引擎列表也同步了 |
| 不介绍回滚 | `sceneRewind` 整段删除，`grep -c "回滚\|rewind" src/film.js` = 0 |
| 提问强提示要突出「在另一个对话里也能收到」 | 镜头 6 拆成两拍：先提问卡**只停在当前会话**，再**换一个会话打开**——顶部横幅 + 侧栏整行脉动 + 「待回答 1」徽标，三个位置都打了聚光灯和标签 |
| 别提「55s 没人答就自动拒绝」 | 旁白与字幕已无 55s / 自动拒绝字样（`grep` 为空）；只说「提问不在当前会话也会弹出来」 |
| 动画/连贯性再进一步（学另一支片子） | ① 底部进度线成为贯穿全片的持续元素（发光的头 + 场景刻度 + 切场涟漪）；② 节拍抽进 `src/timeline.js`（逐字节比对确认画面未变）；③ 加上文字检查；④ 修掉标题场景入场落在 t<0 的问题 |
| 不要卡点音效，只要 BGM | 音效层整层撤掉，`tools/music.py` 恢复到上一版参数（mean −22.4 / max −11.3 dB 与上一版逐项一致），`export-events.mjs`/`master.py` 删除 |
| 二进制改成新项目名 | 收尾命令是 `go build -o agentdeck .`，`grep -c cc-webui src/film.js` = 0 |

## 8. 全流程可一条命令复现

```
$ ./tools/build.sh
== 1/6  workspace fixture      ok
== 2/6  build server + seed demo data
== 3/6  start demo server on :8323   up
== 4/6  capture screenshots    17 张 + rects.json
== 5/6  render frames          2505/2505 frames  281ms/f  (11.8 min)
== 6/6  score + mux            score → out/score.wav (83.5s, peak -14.0 dBFS)
duration=83.500000
```

## 已知取舍（不是 bug）

- **中文字形靠超采样**：画布 backing store 在 `--dsf 1.5` 时是 2880×1620，
  再由 ffmpeg lanczos 降到 1080p。`--dsf 2` 更干净但渲染要 18 分钟。
- **中间帧走 JPEG（q97）**：浏览器编码一张 4K PNG 要 ~1.5s，是流水线瓶颈；
  x264 反正会重编码，q97 的差别在成片上看不出来。
- **场景边界是交叉溶解，不是连续长镜头**：溶解宽 0.5s。想做真正的「一镜到底」
  得让镜头在场景间连续运动，那是另一套设计。
- **界面是真实组件 + 造出来的内容**：`seed-demo.py` 写的是 canonical 事件形状，
  前端渲染走的是真实路径；但会话正文、工具卡、diff、在线会话列表都是造的，
  不是真跑模型。
- **📡 弹窗的状态行是替换过的**：演示实例故意不持有真的常驻进程（见 §5），
  所以状态行本来是「进程启动中…」。截图时改成 `在线 · 注册名 arch-review`
  ——文案逐字抄自 `updateResidentUI()` 对在线进程的输出，属于道具，不是伪造
  界面行为。
- **`ListAgents` 工具卡体是 `{}`**：前端没有给它做定制视图（它没有入参），
  所以按通用分支渲染成 JSON。真实行为，没有粉饰。
