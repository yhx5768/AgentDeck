# 钉住最后输入（Pinned Prompt）实施计划

> 状态：已与需求方逐轮确认方案（悬浮层、折叠/展开、50% 限高、切会话/多端语义）。本文档只做执行拆解，不重新设计。

## 背景

主对话里 assistant 的回复经常很长，用户滚到回复中部/底部时，自己最后发的那条输入早被顶出视口；想回顾"我问的是什么"只能一路往上翻。手机端屏幕小，问题更严重。

## 目标

在消息区顶部提供一个**悬浮**的「当前问题」条：

- 你最后一条输入被滚出视口时自动浮现，滚回来自动消失；
- 默认折叠 2 行，可展开阅读全文（内部滚动，整体不超过消息区半屏）；
- 一键跳回原消息；
- 切会话 / 断线重连 / rewind / 多端同时使用，行为全部正确；
- **纯前端、零持久化、零后端改动**。

## 方案概述（已确认，勿改动）

### 形态

新增独立悬浮元素 `#pinned-prompt`，absolute 定位在新的 `#msg-wrap`（`#messages` 的相对定位父容器）顶部。**不占文档流**，消息区可滚动高度不变，composer 不被挤压。

```
┌ #chat-view ─────────────────────────────┐
│ header / tabs                            │
│ ┌ #msg-wrap（position:relative）───────┐ │
│ │ ╭ #pinned-prompt（absolute 悬浮）──╮ │ │
│ │ │ 你 │ 最后一条输入（2 行截断）  ⌄ │ │ │
│ │ ╰─────────────────────────────────╯ │ │
│ │ ┌ #messages（滚动容器，原样）─────┐ │ │
│ │ │ 回复内容正常滚动，从悬浮条下穿过  │ │ │
│ │ └─────────────────────────────────┘ │ │
│ └──────────────────────────────────────┘ │
│ composer                                 │
└──────────────────────────────────────────┘
```

### 状态模型（核心设计决策：全部派生，零存储）

| 状态 | 来源 | 跨端同步 |
|---|---|---|
| 内容（钉哪条） | DOM 中最后一条 `.msg-user` 的 `textContent` | ✅ 免费——SSE/回放渲染天然一致 |
| 可见性 | 本端 `IntersectionObserver`（root = `#messages`） | ❌ 故意不同步（滚动位置是每端自己的） |
| 展开/折叠 | 本端内存瞬态 | ❌ 不同步、不进 localStorage；切会话/新消息即重置为折叠 |

服务器不知道悬浮条的存在；无事件 schema 变化。

### 显隐规则

- 最后一条 `.msg-user` **滚出视口顶部** → 悬浮条淡入（opacity + 轻微位移，~150ms）；
- 重新可见 → 自动淡出；
- 判定方向：observer 报不可见时检查 `target.getBoundingClientRect().top < root.getBoundingClientRect().top`，只有从**上方**滚出才显示（防止目标在视口下方时误显示）；
- 会话无 user 消息 → 保持隐藏；
- 切到「轨迹」tab → 隐藏（见步骤 4 的 switchChatTab 改动）。

### 折叠 / 展开

- **折叠态（默认）**：`line-clamp: 2`（PC/手机一致）；
- **展开按钮条件显示**：内容不超过 2 行截断（`scrollHeight <= clientHeight`）时不渲染展开按钮；只有真实溢出才出现——单行短问不再有按钮；
- **展开态**：头行（tag + 展开按钮）固定，正文 `.pp-text` 独立滚动：
  - 整体 `max-height: 50%`（相对 `#msg-wrap`，即"主对话半屏"，PC/手机同一规则，不用 vh、不用 JS 算）；
  - `.pp-text`: `overflow-y: auto` + `overscroll-behavior: contain` + `touch-action: pan-y`（手机端条内滑到底不带动消息区）；
  - 正文可滚且未滚到底时显示**底部渐隐遮罩**，滚到底消失；
  - 折叠↔展开用 `max-height` + opacity transition；收起时正文滚动位置复位到顶；
- 展开后 textContent 可选中复制。

### 交互分配

| 动作 | 折叠态 | 展开态 |
|---|---|---|
| 点文字区 | 平滑滚回原消息（`scrollIntoView`） | 选中/复制，不跳转 |
| `⌄ / ⌃` 按钮 | 展开（仅内容超 2 行截断时显示） | 收起 |

> 需求方确认：悬浮条只展示 tag + 文本，**不保留任何跳回/回滚式按钮**（原设计头行的 ↩ 按钮已移除）。跳回原消息只有折叠态点文字一个入口。

### 换钉规则

- 点发送（乐观气泡 `appendOptimisticUser` 渲染那一刻）即换钉；
- 服务端 `user` 事件到达（含多端其他端发的）→ 换成权威气泡并换钉；
- **排队中的 pending 消息不算**"最后的输入"（pending-bubble 在 composer 上，不渲染为 `.msg-user`），发出后自动换钉；
- **peer 消息（`msg-peer`）不参与**；
- 换钉时重置为折叠态、正文滚动复位；
- rewind 截断后重新钉剩余的最后一条；全部删完 → 隐藏。

### 视觉规格

| | PC | 手机（≤640px，沿用现有断点） |
|---|---|---|
| 宽度 | `min(880px, 100% - 48px)`，居中（对齐消息列） | `calc(100% - 16px)` |
| 位置 | 消息区顶部下 8px | 顶部下 6px |
| 背景 | `var(--pin-bg)`（accent 蓝 tint）85% 透明 + `backdrop-filter: blur(8px)` + accent 蓝边框 `var(--pin-border)` + `var(--shadow-sm)` 级阴影 | 同左 |
| z-index | 30（高于消息内容与 traj-toolbar(5)/composer(10)，低于 drawer/popover(45+)/modal(100)） | 同左 |
| 图标 | `chevron-down` / `chevron-up`（`corner-up-left` 已随 ↩ 按钮移除） | 同左 |
| 动效 | 遵循现有 `@media (prefers-reduced-motion: reduce)` 约定 | 同左 |

## 涉及模块 / 文件

| 文件 | 改动 |
|---|---|
| `web/index.html` | `#messages` 外包 `#msg-wrap`；新增 `#pinned-prompt` 结构 |
| `web/style.css` | `#msg-wrap` 布局 + 悬浮条全部样式（含移动端、reduce-motion） |
| `web/app.js` | 悬浮条逻辑模块 + 5 处接线 |

**无任何 Go 改动**（`go:embed` 自动带上新前端文件）。

## 数据 / 调用链变化

无后端与协议变化。纯前端调用链：

```
发送消息                SSE user 事件（本端/他端）     rewind 广播            切会话 / SSE 重连
   │                        │                         │                      │
appendOptimisticUser    renderEvent(user 分支)     applyRewindInUI        openSession: hidePinnedPrompt()
   │                        │                         │                      │  (innerHTML 清空前摘 observer)
   └──── refreshPinnedPrompt() ───────────────────────┘                   connectEvents → replay_end
              ▲                                                             flushHistory() 末尾
              └──────────────────── refreshPinnedPrompt() ───────────────────┘

refreshPinnedPrompt()（幂等）：
  observer.disconnect() → 找 #messages 最后一条 .msg-user
    → 无：隐藏             → 有：更新文本 / 复位折叠态 / 重挂 observer
observer 回调：目标从上方滚出 → 显示；重新可见 → 隐藏
```

## 实施步骤

### 步骤 1：HTML 结构

**改什么**：
- `web/index.html` 中把 `<div id="messages"></div>`（约 115 行）包进 `<div id="msg-wrap">`；
- 在 `#msg-wrap` 内、`#messages` 之前插入 `#pinned-prompt`：

```html
<div id="msg-wrap">
  <div id="pinned-prompt" class="hidden">
    <div class="pp-head">
      <span class="pp-tag">你</span>
      <button id="pp-expand" class="pp-expand icon-btn" title="展开"><i data-lucide="chevron-down"></i></button>
    </div>
    <div class="pp-text" id="pp-text"></div>
    <div class="pp-fade hidden"></div>
  </div>
  <div id="messages"></div>
</div>
```

**为什么**：悬浮条需要滚动容器外的相对定位锚点（放 `#messages` 内会跟着滚走；放 `#chat-view` 下 top:0 会盖住 header/tabs）。

**注意事项**：
- `#traj-view` 保持 `#chat-view` 的直接子元素，不进 wrapper；
- 已核对 `#messages` 的全部引用（app.js 13 处、style.css 4 处）：均为 id 选择器，无父子结构选择器依赖，加 wrapper 安全；
- `#load-more-wrap` 是 `#messages` 内部首位子元素，随 `#messages` 整体进 wrapper，无需改动。

### 步骤 2：CSS

**改什么**（`web/style.css`）：
- `#msg-wrap { position: relative; flex: 1; min-height: 0; display: flex; flex-direction: column; }`——`flex:1` 从 `#messages` 移到 wrapper；`#messages` 保留 `flex:1; min-height:0; overflow-y:auto` 及其余原样式；
- `#pinned-prompt`：absolute 定位 + 上文视觉规格（宽度/背景/blur/阴影/z-index 30/圆角沿用 `var(--r-2xl)`）；`display: flex; flex-direction: column`；
- `.pp-text`：折叠 `line-clamp: 2`；展开 `.expanded .pp-text { overflow-y: auto; flex: 1; min-height: 0; -webkit-line-clamp: unset; }`；
- `.pp-fade`：底部渐隐遮罩（linear-gradient → transparent），仅 `.expanded` 且未滚到底时显示；
- 显隐动画：`.hidden` 之外用 `opacity/translateY + visibility` 过渡（`hidden` 类只做布局级隐藏，显隐动画用单独的 `.pp-show` 类或直接 opacity/visibility 双属性 transition）；
- 移动端 `@media (max-width: 640px)`：宽度/top 调整；
- 在既有 `@media (prefers-reduced-motion: reduce)` 块（约 2219 行）里把新 transition 一并关掉。

**为什么**：50% 限高用百分比相对 `#msg-wrap`（flex 子项有确定高度，百分比可解析），天然满足"主对话半屏"且键盘弹起时自适应。

**注意事项**：
- `backdrop-filter` 需要有半透明底色才生效；
- 深浅主题都用 CSS 变量，不写死颜色；
- line-clamp 需带 `-webkit-` 前缀三件套（`display:-webkit-box; -webkit-box-orient:vertical`）。

### 步骤 3：JS 核心逻辑（`web/app.js` 新增一节）

**改什么**——新增约 80~120 行，含：

```js
const pinnedPrompt = {
  observer: null,      // IntersectionObserver 实例
  target: null,        // 当前盯着的 .msg-user 节点
  expanded: false,
};
```

- `refreshPinnedPrompt()`（幂等）：摘旧 observer → 取 `$('#messages').querySelectorAll('.msg-user')` 最后一条 → 无则 `hidePinnedPrompt()`；有则更新 `#pp-text` 的 `textContent`、复位折叠/滚动/渐隐、对目标重新 `observe`；
- `hidePinnedPrompt()`：摘 observer + 隐藏 + 复位折叠态；
- observer 回调：`isIntersecting` → 隐藏；不可见且 `rect.top < rootRect.top` → 显示；**额外 gate**：`#msg-wrap` hidden（轨迹 tab）或 `#chat-view` hidden 时一律不显示；
- 展开/收起 toggle：切 `.expanded` class、换 chevron 图标、`renderIcons` 重渲、收起时 `pp-text.scrollTop = 0`；
- 跳回：折叠态点 `.pp-text` → `pinnedPrompt.target.scrollIntoView({ behavior: 'smooth', block: 'start' })`（observer 随后自动隐藏悬浮条；无独立跳回按钮）；
- `.pp-text` scroll 监听：可滚且未到底（`scrollTop + clientHeight < scrollHeight - 4`）显示 `.pp-fade`，否则隐藏；
- 折叠态点 `.pp-text` → 跳回；展开态点文字不跳转（事件按 `expanded` 分流）；
- 初始化时 `renderIcons($('#pinned-prompt'))`。

**为什么**：单一幂等重算入口，任何时序（回放/重连/多端事件乱序到达）调重了都安全；显隐完全由 observer 派生，无状态可腐烂。

**注意事项**：
- `textContent` 会带上附件 chip 的文件名（图片 thumb 无文本）——可接受，文件名本身有信息量，不做剥离；
- observer 的 root 用 `$('#messages')`，threshold 0 即可；
- 不要在 scroll 事件里做显隐判定（observer 已够），scroll 监听只用于 `.pp-fade`。

### 步骤 4：接线（5 处，每处一行）

| # | 位置 | 调用 | 为什么 |
|---|---|---|---|
| 1 | `renderEvent` user 分支末尾（`app.js:2383` 附近） | `refreshPinnedPrompt()` | 权威气泡替换乐观气泡后换钉；同时覆盖多端收到的 user 事件 |
| 2 | `appendOptimisticUser` 末尾（`app.js:3158` 附近） | `refreshPinnedPrompt()` | 点发送即刻换钉 |
| 3 | `flushHistory()` 末尾（`app.js:1406` `scrollBottom()` 之后） | `refreshPinnedPrompt()` | 覆盖切会话 / SSE 重连 / 首次打开三个入口；**必须在 optimistic 节点重挂回末尾之后调用** |
| 4 | `applyRewindInUI` 截断后（`app.js:1758` `syncTaskPanel()` 之后） | `refreshPinnedPrompt()` | 被钉节点可能已被 remove，重钉剩余最后一条 |
| 5 | `openSession` 清 `innerHTML` 之前（`app.js:1074` 前） | `hidePinnedPrompt()` | 先摘 observer 再销毁节点，防悬空引用与旧会话文本闪现 |

另外把 `switchChatTab`（`app.js:1119`）里 `$('#messages').classList.toggle('hidden', isTraj)` 改为对 `$('#msg-wrap')` 切换——wrapper 隐藏时悬浮条（其子元素）一并隐藏；同时这也消除了"display:none 导致 observer 报不可见而误显示"的问题（配合步骤 3 的 gate 双保险）。

**注意事项**：
- `resetChatForReplay`（`app.js:1287`）也做 `innerHTML = ''`——它保留 optimistic 气泡重挂，且随后必经 `flushHistory`（调用点 3）恢复悬浮条，**不需要**单独加 hide 调用；若实测出现旧文本闪现再补；
- load-more（prepend 历史）不改变最后一条消息，**不接线**。

### 步骤 5：构建 + 验证

见下节。

## 明确不改的内容（防止范围蔓延）

- **不改任何 Go 代码**：无新 API、无事件 schema 变化、无持久化字段；
- **不写 localStorage / notes.json / db.json**：三类状态全部派生（见状态模型表）；
- **不做跨端可见性/展开态同步**：这是刻意决策，不是遗漏；
- **不改 `.msg-user` 气泡本体**：不回填内容、不加 sticky、不动 rewind 按钮逻辑；
- **不改 pending-bubble / 任务面板 / 轨迹投影**：pending 消息不钉是刻意决策；
- **不处理 peer 消息**（`msg-peer` 永不成为钉）；
- 不加设置开关 → 见 DECISION NEEDED；
- 不做 ESC 快捷键、拖拽移动悬浮条等额外交互。

## 验证方案

### 编译 / 静态检查

```bash
go build -o cc-webui .   # embed 带新前端文件，确认编译过
go vet ./...
go test ./...            # 仅 internal/claudehist 有测试，本功能不触及，跑一遍确认无回归
```

### 前端 harness 验证（参照 `web/_notes_harness.html` 模式）

- 用真实 transcript（`sed` 抽取 + node 驱动）灌出"长回复"场景，headless Chrome 截图确认：折叠态样式、展开态 50% 限高、渐隐遮罩、深浅两个主题、移动端视口（≤640px 模拟）。

### 运行验证（起真实实例）

```bash
./cc-webui -listen 127.0.0.1:8321
```

手动场景矩阵：

| # | 场景 | 预期 |
|---|---|---|
| 1 | 发一条消息，回复很长，往下滚 | 悬浮条淡入，2 行折叠 |
| 2 | 滚回顶部让原消息可见 | 悬浮条自动消失 |
| 3 | 展开 → 粘贴过 5k 字长输入 | 整体 ≤ 消息区半屏，正文内部滚动，渐隐遮罩滚到底消失 |
| 4 | 折叠态点文字（↩ 按钮已随偏差 #6 移除，跳回仅此入口） | 平滑滚回原消息，悬浮条随后自动消失 |
| 5 | 展开态选中复制文字 | 可复制，不触发跳转 |
| 6 | 展开态时（多端）另一端发新消息 | 就地换钉为新消息，复位折叠态 |
| 7 | 连续快速发多条 | 始终钉最后一条 |
| 8 | rewind 掉被钉的消息 | 钉上一条 user 消息；全部回滚完则隐藏 |
| 9 | 切到轨迹 tab 再切回 | tab 期间悬浮条不出现；切回后按当前滚动位置正确显隐 |
| 10 | 切走会话再切回（长回复会话） | 落底部 → 悬浮条自动浮现 |
| 11 | 断网/SSE 重连（手机锁屏解锁） | 重放后悬浮条恢复正确 |
| 12 | 两个浏览器同开一会话，一端滚动 | 各端显隐独立，内容一致 |
| 13 | 常驻会话收到 peer 消息 | 不换钉 |
| 14 | 手机端：条内滑动到底 | 不带动消息区滚动（overscroll 隔离） |
| 15 | 手机端键盘弹起 | 悬浮条仍 ≤ 半屏，不糊死屏幕 |
| 16 | 空会话 / 新会话 | 悬浮条不出现 |
| 17 | load-more 翻历史 | 悬浮条内容不变、行为正常 |
| 18 | 单行 / 恰好 2 行整的短输入 | 展开按钮不出现；仅内容超 2 行截断时出现（新增） |

### 重点边界

- **observer 方向判定**：目标在视口**下方**（如会话刚打开尚未滚到底的极端布局）不得显示悬浮条；
- **display:none 与 observer**：切 tab 时目标不可见，必须靠 `#msg-wrap` hidden gate 防误显示；
- **flushHistory 调用顺序**：必须在 optimistic 节点重挂之后取"最后一条"，否则换钉目标错误；
- **rewind 后 observer 目标已销毁**：`refreshPinnedPrompt()` 必须先 disconnect 再重挂，不能复用旧 target；
- **展开态换钉**：新消息到达时若正处于展开态，复位为折叠（避免展示错位旧文本）。

## 未决项（TODO / DECISION NEEDED）

- **DECISION NEEDED：头行字数统计**（"你 · 共 812 字"）。设计稿 mockup 中出现过但未经需求方明确确认。实现成本极低（`textContent.length`），默认建议保留；若不要，删除 tag 行中对应 span 即可。
- **DECISION NEEDED：是否需要设置开关**（设置弹窗里加"显示当前问题悬浮条"开关，存 localStorage）。讨论中未提及，默认不做；如后续有需求再加，不影响本期结构。
- **TODO（验证时确认）**：`resetChatForReplay` 清 DOM 的瞬间是否会出现旧悬浮条文本闪现；若出现，在该函数开头补一行 `hidePinnedPrompt()`。

## Implementation Checklist

- [x] 1. `index.html`：`#messages` 包入 `#msg-wrap`，新增 `#pinned-prompt` 结构（含 pp-tag / pp-text / pp-expand / pp-fade；pp-jump 随偏差 #6 移除）
- [x] 2. `style.css`：`#msg-wrap` 布局；`flex:1` 从 `#messages` 移到 wrapper
- [x] 3. `style.css`：`#pinned-prompt` 基础样式（定位/宽度/背景 blur/阴影/z-index 30/圆角/显隐过渡）
- [x] 4. `style.css`：折叠 2 行 line-clamp；展开态 `max-height: 50%` + `.pp-text` 内部滚动 + 滚动隔离 + `.pp-fade` 渐隐
- [x] 5. `style.css`：移动端 `@media (max-width: 640px)` 规格；`prefers-reduced-motion` 覆盖
- [x] 6. `app.js`：`pinnedPrompt` 状态对象 + `refreshPinnedPrompt()`（幂等）+ `hidePinnedPrompt()`
- [x] 7. `app.js`：IntersectionObserver 显隐（含方向判定 + `#msg-wrap`/`#chat-view` hidden gate）
- [x] 8. `app.js`：展开/收起 toggle（换图标 + 收起复位滚动 + 折叠点击分流跳回）；展开按钮按内容溢出条件显示（`updatePpExpandBtn()`，见偏差 #7）
- [x] 9. `app.js`：`.pp-text` scroll 监听驱动 `.pp-fade`（与展开动作共用 `updatePpFade()`，避免短文本残留遮罩）
- [x] 10. 接线①：`renderEvent` user 分支末尾（app.js:2383）
- [x] 11. 接线②：`appendOptimisticUser` 末尾（app.js:3158）
- [x] 12. 接线③：`flushHistory()` 末尾（optimistic 重挂之后，app.js:1406）
- [x] 13. 接线④：`applyRewindInUI` 截断后（app.js:1758）
- [x] 14. 接线⑤：`openSession` 清 DOM 前 `hidePinnedPrompt()`（app.js:1075）
- [x] 15. `switchChatTab` 的 hidden 切换从 `#messages` 改为 `#msg-wrap`（app.js:1120）
- [x] 16. `go build` + `go vet` + `go test ./...` 全绿
- [x] 17. harness + headless 截图：折叠/展开/渐隐/双主题/移动视口/单行短文本（`docs/_pinned_harness.html`，DOM 探针 + 截图双验证；截图已按新结构重拍存 `docs/screenshots/pinned-{all,mobile}.png`）
- [ ] 18. 手动场景矩阵 #1–#18 逐项过（headless 已自动化覆盖 #1/2/3 折叠展开/6/7 换钉/8 rewind/9 tab gate/10/11 时序/16 空会话/18 按钮条件显隐与 UI 文字隔离；点文字 smooth 滚动跳回 #4、复制 #5、多端 #12/13、手机手感 #14/15、load-more #17 需真人真机确认）
- [x] 19. 确认 DECISION NEEDED 两项（字数统计**去掉**——实现后用户看实际效果明确不要；设置开关不做）
- [x] 20. 验证 TODO：`resetChatForReplay` 开头已补 `hidePinnedPrompt()`（app.js:1287），无闪现风险

### 实现中发现并修复的偏差（验证时探针抓到）

0. **悬浮条文本混入 UI 文字**：rewind 按钮（「回滚」字样）append 在 `.msg-user` 直接子级，而取文本用整条 `.msg-user` 的 `textContent` → 悬浮条尾部出现「回滚」。改为只取 `.bubble` 的 `textContent`（消息正文与附件 chip 都在 bubble 内；role-tag/rewind 按钮在 bubble 外，自然排除）。同时按用户反馈移除头行字数统计（`.pp-count` span 及相关 JS/CSS 全删），`.pp-tag` 改 `margin-right: auto` 继续右推按钮组。
1. **index.html 按钮缺 id**：`pp-jump`/`pp-expand` 只有 class，而 app.js 用 `$('#pp-jump')`/`$('#pp-expand')` 绑定 → null 上 addEventListener 抛错、模块初始化崩。已补 id。
2. **折叠态 `.shown` 未覆盖基础 `max-height: 0`**：DOM 探针实测折叠显示时 ppH=14（内容全被裁掉）。`.shown` 补 `max-height: 104px`（折叠过渡端点）+ 显式 `#pinned-prompt.shown.expanded { max-height: 50% }`（同特异性按文档序后者胜，否则展开态会被 104px 盖掉）。
3. **`.shown` 缺 `pointer-events: auto`**：基础态 `pointer-events: none` 未恢复 → 悬浮条可见但点不动。
4. **hide/show 竞态**：`hidePinnedPrompt` 的 180ms 延时加 `hidden` 定时器未被 `showPinnedPrompt` 清除——快速滚出又滚回时 hidden 在 show 之后补上，悬浮条再也看不见。`showPinnedPrompt` 开头补 `clearTimeout(pinnedPrompt.fadeTimer)`。
5. **短文本展开残留渐隐遮罩**：expand 处理器无条件显示 `.pp-fade`，而遮罩显隐只在 scroll 事件里重算；内容不可滚时永不触发 scroll → 遮罩残留。抽出 `updatePpFade()`，展开/滚动/换钉共用。
6. **头行 ↩（`pp-jump`）按钮移除**：用户看展开态弹出的 corner-up-left 按钮形似回滚，明确「这里只展示文本」。删 index.html 按钮 + 全部 JS toggle/监听 + CSS 规则 + lucide wanted 列表项；跳回只剩折叠态点文字一个入口。
7. **展开按钮条件显示**：折叠态内容不超过 2 行截断时不出现展开按钮。实测 Chrome `-webkit-line-clamp: 2` 下 `scrollHeight` 返回全文高（1 行/恰好 2 行 `scrollHeight==clientHeight`，3 行以上多出 1 行高）→ 用 `updatePpExpandBtn()` 以 `scrollHeight - clientHeight > 1` 判定；因 `.hidden` 是 `display:none`（子树不可布局，两值皆 0），换钉发生在悬浮条隐藏期间会误藏按钮——`showPinnedPrompt()` 现身时再校正一次。
8. **「你 {时间}」前缀是旧前端残留，非现码缺陷**：`.bubble` 提取（偏差 #0）后文本已不含 role-tag 与 msg-time；用户仍看到「你 HH:MM」开头说明浏览器缓存了旧 app.js / 实例未重建。另把无 `.bubble` 结构的兜底路径改为克隆后剔除 `.role-tag/.msg-time/.rewind-btn` 再取文本，双保险。
9. **悬浮条配色与工具调用块撞色**：原表面 `bg2 85% + border` 与 `.tool-block`（工具调用，`bg2 + border`）几乎同色，一眼分不出。新增每主题变量 `--pin-bg`（`color-mix(accent 10%, bg2)`）/ `--pin-border`（`color-mix(accent 45%, border)`），底色、边框、`.pp-fade` 渐隐终点、无 blur 兜底全部改走新变量。computed 实测（含 alpha 合成）两主题下悬浮条与工具块色差均显著（蓝 tint vs 中性灰，边框呈 accent 蓝）；与用户气泡同属蓝族但更浓一档——语义一致（都是「你」的内容）且位置/头行/边框可辨。验证细节：computed 背景色有两种格式（`color(srgb 0-1 浮点)` 与 `rgb() 0-255 整数`），对比前须归一；半透明底色对比要按所在背景合成后再比，直接比 raw rgba 会把「浅蓝近白」的 7% 用户气泡误判为深蓝。

### 验证方法备注（下次复用）

- 本机 headless 验证**必须带 `--virtual-time-budget`**，但虚拟时间下 CSS transition 会冻结在起点（computed style 读不到终态）——交互 harness 注入 `transition: none !important` 测状态机，动画/视觉用静态 harness 截图验证。
- DOM 探针（computed style 写进页面元素 + `--dump-dom` grep）比截图可靠，静态 harness 每 case 一行探针数据。
