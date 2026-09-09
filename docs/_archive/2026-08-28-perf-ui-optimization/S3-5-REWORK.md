# S3-5 打回重做：图标独用按钮缺 aria-label

## 背景

S3-5「无障碍补齐」已在 `85f3ec9` 提交过一轮，但只补了一部分图标按钮。
复查已落盘文件后确认：**仍有 13 个「图标独用」按钮缺 `aria-label`**。

S3-5 的验收标准是四类问题「在对应元素上均已补齐」，改法要点里的
「如 `:408-422` 的上移/下移/编辑/复制/删除、`:754-774` 的三个监控操作」
是举例（「如」），不是穷举清单。因此这一条尚未达成验收。

另外三类已确认达成，**不要重复改动**：
- 侧栏 / 移动导航 `role="tablist"` / `role="tab"` / `aria-selected`：已补齐。
- 表头 `scope="col"`：29 个 `<th>` 全部已有，无遗漏。
- 可聚焦 chip 的 `aria-describedby="immediate-tooltip"`：4 处已补，
  tooltip 容器 `id="immediate-tooltip"` 已存在。

## 重要：本轮必须改源文件，不是改产物

S3-1 已经落地（提交 `f0bff82`），`cmd/gateway/web/index.html` 现在是
**构建期产物**，直接改它会被 CI 的一致性闸门打回。

- 要改的文件：`cmd/gateway/web/src/index.template.html`
- 改完必须重新生成产物：`make web-html`
- 自检：`go run ./cmd/webbuild -check` 必须 exit 0

产物与源码不一致时 `-check` 会打印「请运行 `make web-html` 后提交产物」并以
exit 1 失败，CI 拿它当闸门。

## 待补清单（行号基于 `src/index.template.html` 当前状态）

以下 13 个按钮内部只有一个 `<span class="material-symbols-outlined">`，
没有任何可见文字，屏幕阅读器会把 ligature 原文（如 `content_copy`）念出来。

| 行号 | 图标 | 现有 title | 建议 aria-label |
|------|------|-----------|----------------|
| 63   | `menu` | 打开导航 | 打开导航 |
| 129  | `close` | 无 | 关闭导航 |
| 549  | `edit` | 编辑 Provider | 编辑 Provider |
| 552  | `content_copy` | 复制为新 Provider | 复制为新 Provider |
| 555  | `delete` | 删除 Provider | 删除 Provider |
| 925  | `refresh` | 刷新 | 刷新请求日志 |
| 928  | `filter_alt_off` | 清空筛选 | 清空日志筛选 |
| 1134 | `close` | 无 | 关闭弹窗 |
| 1195 | `close` | 无 | 关闭弹窗 |
| 1226 | `search` | 动态 `:title` | 动态，见下 |
| 1234 | `keyboard_arrow_up` | 上移 | 上移 |
| 1237 | `keyboard_arrow_down` | 下移 | 下移 |
| 1240 | `delete` | 删除该候选 | 删除该候选 |

### 两处需要注意的细节

1. **L1226 的 title 是动态表达式**
   现状是 `:title="target.provider ? '查询该 Provider 的上游模型列表' : '请先选择 Provider'"`。
   这里应该用 `:aria-label` 跟上同样的两分支表达式，**不要**写成静态
   `aria-label`——静态值会在「未选 Provider」时给出误导性的播报。
   仓库里已有同样写法的先例（监控页三个动作按钮用的就是 `:aria-label`
   跟随状态），照那个风格写。

2. **L63 / L129 / L1134 / L1195 是成对的开关与关闭按钮**
   L63 是移动导航的汉堡按钮，L129 是抽屉内的关闭按钮；
   L1134 / L1195 分别是 Provider 弹窗与 Route 弹窗的关闭按钮。
   L129 / L1134 / L1195 当前连 `title` 都没有，`aria-label` 是唯一可访问名，
   必须补。弹窗的两个关闭按钮建议写得能区分上下文
   （如「关闭 Provider 弹窗」/「关闭路由弹窗」），不要都写「关闭」。

## 约束

1. **只加无障碍属性，不改任何交互行为**（S3-5 接口约束）。
   不要改 class、不要改 `@click`、不要动 `:disabled`、不要调整 DOM 结构。
2. **已有 `title` 的保留**，`aria-label` 是新增而非替换。
   `title` 提供鼠标悬停提示，`aria-label` 提供无障碍名，两者职责不同。
3. **不要顺手清理无关代码**。`index.template.html` 里的注释记录了具体 bug
   成因（表单恢复污染、`:style` 覆写 `x-show`、字面 NUL 字节让 grep 失效等），
   一律不得删除。
4. 注释用中文，与仓库风格一致。
5. 不新增配置字段，不改 Go 代码（本轮纯前端属性补齐）。

## 验收自检（全部必须通过）

```bash
# 1. 重新生成产物并确认一致
make web-html
go run ./cmd/webbuild -check        # 必须 exit 0

# 2. 确认 13 个按钮都补上了：下面这条命令应输出 0
#    （统计「图标独用且缺 aria-label」的 button 数量）
python3 - <<'PY'
import re
src = open('cmd/gateway/web/src/index.template.html', encoding='utf-8', errors='replace').read()
n = 0
for m in re.finditer(r'<button\b', src):
    depth_start = m.start()
    end = src.find('</button>', depth_start)
    if end < 0:
        continue
    block = src[depth_start:end]
    tag_end = block.find('>')
    tag = block[:tag_end]
    if 'aria-label' in tag:
        continue
    if 'material-symbols-outlined' not in block:
        continue
    # 去掉所有标签后看还有没有可见文字
    inner = re.sub(r'<[^>]*>', '', block[tag_end+1:])
    inner = re.sub(r'\s+', '', inner)
    # ligature 字体的图标名本身在 span 里，已被上面剥掉；
    # x-text 渲染的文字不在源码里，靠 x-text 属性判断
    if inner or 'x-text' in block[tag_end+1:]:
        continue
    n += 1
print(n)
PY

# 3. Go 侧回归（容器内跑）
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine gofmt -l ./cmd ./internal
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go vet ./...
docker run --pull never --rm --name ai-gateway-dev-verify -v "$PWD":/work -w /work golang:1.23-alpine go test ./...
```

第 2 步的脚本输出必须是 `0`。如果你认为某个按钮不该补（例如它其实有可见
文字、我判断错了），**不要静默跳过**：在回复里写明是哪一个、为什么，
让我复核。

## 不要做的事

- 不要 `git commit`（提交由我确认后执行）。
- 不要改 `cmd/gateway/web/index.html` 以外的产物文件，也不要手改产物——
  只改 `src/`，然后 `make web-html`。
- 不要声称达成 WCAG 合规。S3-5 明确说完整结论需辅助技术实测与人工评审，
  本任务只交付这几处明确修正。
