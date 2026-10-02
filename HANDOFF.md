# Codex / Zcode 交接板

更新时间：2026-10-01

## 使用方法

这是两个 Agent 切换工作的唯一交接点。每轮开始先刷新本文件；每轮结束必须把结果写回来。不要把聊天记录当作唯一上下文。

状态只使用：`IDLE`、`IN_PROGRESS`、`DONE`、`BLOCKED`。

## 当前认领

| Agent | 状态 | 本轮任务 | 独占文件/目录 | 开始时间 |
|---|---|---|---|---|
| Codex | DONE | Android/Windows v0.7.6 预览版与网站下载已发布并实际验哈希；真机待验项保留 | 无 | 2026-10-01 |
| Zcode | IDLE | 无 | 无 | - |
| Claude（云端） | DONE | 状态优先界面第二轮已完成（分支 `claude/ui-status-first`，未发布，待真机验收） | 无 | 2026-10-01 |

认领规则：

1. 开工前填写自己的行，并保存本文件。
2. 再读一次对方的行；文件范围重叠时不得开工。
3. 公共协议、Gradle 根配置、README 和本文件视为共享文件，同一时刻只能由一个 Agent 修改。
4. 完工后将状态改为 `DONE` 或 `BLOCKED`，释放“独占文件/目录”，并在下方追加一条简短记录。

## 当前基线

- 源码来源：`XgyLanSms-source-v0.6.1.zip` 的干净副本；未包含构建缓存、APK、EXE、`node_modules`、Wrangler 登录信息或密钥。
- Android：v0.7.6 / versionCode 16，布局与无效唤醒改进已发布，后台教程和历史功能保留。
- Windows：v0.7.6，普通合并通知与验证码自动复制已发布；真实桌面交互待验。
- Relay：旧 `/v1/*` v0.6.0 兼容逻辑保留；本地 v0.7.0 候选增加 D1 账号 API 与 Web。
- 已知部署：`https://msgdock.dpdns.org`，部署 ID `1e3ef438-2086-4ca7-9bd4-17e9b5c2d017`；主备下载页面均已现场验证。
- 本轮基线验证：Android 59 项 JVM、SDK 构建与签名通过；Windows test/vet/GUI 构建通过；网页 12 项、typecheck、dry-run 和线上下载检查通过。Gradle lint 未运行，不能沿用历史版本的 lint 结论。
- 未完成：没有连接真实 Android/ADB 设备，因此恢复、锁屏、HyperOS 和真实 SMS 端到端仍需实机验收。

## 建议任务池

| 优先级 | 任务 | 推荐独占范围 | 前置条件 |
|---|---|---|---|
| P0 | Android 实机验收并记录可复现结果 | 先不改代码；必要时只改 `BUG.md` | 有测试手机与 ADB |
| P1 | 修复实机发现的恢复/配对状态问题 | `app/` 中明确到文件 | 先有复现证据 |
| P1 | Windows Toast/历史/开机启动回归 | `windows/` | Windows 10/11 |
| P2 | 设计 ACK 后云历史浏览/恢复 | 先只改 `DESIGN.md` | 用户确认需求与保留策略 |

## 交接记录

### 2026-10-02 / Claude（云端）/ DONE — 第三轮：首次使用走查与自审（分支候选，未发布）

- 按新用户视角逐步走查两端代码，修复首屏重复、平板死胡同、空状态误导等问题，并自审第二轮未经真机验证的部分。
- Android：状态卡带第一项待处理按钮，列表不再重复同一项；无短信功能的设备登录后提示“开启接收”；最近短信空状态如实说明只显示收到的短信；“去设置”不再把表单滚出屏幕。修复电池优化设置页缺失时可能崩溃、按钮触控区不足 48 dp、200% 字体下链接被截断、描边几乎看不见。
- Windows：再次打开时可接替正在退出的实例；退出时取消进行中的登录和配对请求；读取系统通知设置，被关闭时进入待处理；通知 COM 类启动即注册；最小化窗口可由托盘恢复；首次关窗弹一次托盘提示；复制后状态栏确认；配对码倒计时改为中文；修复图标绘制失败时托盘无图标、`App.cloud` 数据竞争。
- 验证：Android 98 项 JVM 测试通过；Windows `GOOS=windows` vet、测试编译和 GUI 构建通过，35 项纯逻辑测试在 Linux 镜像包中以 `-race` 通过。
- 待真机：Windows 关闭通知后待处理项是否出现、重启后点击旧通知卡片、首次关窗气泡、最小化恢复、退出中再次打开的接替时机；Android 平板“开启接收”流程、OEM 系统电池设置跳转、200% 字体下的状态卡按钮。

### 2026-10-01 / Claude（云端）/ DONE — 五态状态模型与界面第二轮（分支候选，未发布）

- 分支 `claude/ui-status-first`。没有修改 main、release 分支或标签，没有部署网站，没有发布安装包。版本号改为 Android 0.7.7（versionCode 17）、Windows 0.7.7，仅作为候选标记。
- `DESIGN.md` 新增“同步状态模型”：五种状态、阈值（2 倍轮询间隔，最少 30 秒）、连续失败 5 分钟判中断、待处理项顺序和统一命名。Android `SyncState` 与 Windows `evaluateSyncState` 都按此实现，替换了第一轮的 `SyncHealth` 和 `desktop_status.go`。
- 同步代码只增加只读记账：成功时间、连续失败起点、请求进行中计数、最近转发和接收来源、当前轮询间隔。没有改变轮询间隔、退避、LAN v1、端到端加密和先落盘再 ACK。旧配对云端的空轮询现在也算一次成功，否则空闲时会一直显示“同步延迟”。
- Android：首页重排为状态卡、待处理、最近 5 条、接收端（局域网与云端合并）、账号（折叠）、高级（折叠，含原状态文本）。文字三级样式，按钮加波纹，青色只用于主按钮和链接。通知增加“复制验证码”按钮（`OtpCopyReceiver`，不导出）。
- Windows：托盘彩色圆点图标，窗口分“概览”“设置”两页，最近短信用表格，Toast Tag 按发件人哈希，附注设备名和时间，隐私预览三档写入配置（缺省为完整）。登录、注册、退出成功不再弹窗。
- 验证：Android 用 API 36 框架包做类型检查，96 项 JVM 测试通过；Windows `GOOS=windows` 下 vet、测试编译和 GUI EXE 交叉编译通过，27 项纯逻辑测试在 Linux 镜像包中通过。未运行 Gradle、lint、模拟器和 Windows 上的完整 `go test`。
- 修复 Windows“明明没在运行却提示已在运行”：再次打开 EXE 时改为唤出已运行实例的窗口（命名事件 `Local\XgyLanSmsReceiver.Show`），开机自启的 `--tray` 启动静默退出；只有旧版实例在运行时才弹窗，并说明去托盘退出或在任务管理器结束。退出清理超过 10 秒时强制结束，避免挂起的进程一直占着单实例锁。
- 待真机：360 dp 与 200% 字体排版、按钮波纹、通知复制按钮；Windows 100% 和 150% DPI 布局、托盘圆点在浅色和深色任务栏上的显示、表格右键与双击、按发件人分组的通知中心表现。

### 2026-10-01 / Claude（云端）/ DONE — 状态优先的首页与通知（分支候选，未发布）

- 基于 `claude/android-power-keepalive`（上一轮 Claude 会话的省电改动，此前未验证、未写交接）继续，分支 `claude/ui-status-first`。没有修改 main、release 分支或标签，没有部署网站、没有发布安装包。
- 省电分支复核：代码逻辑与 DESIGN 中“Android 省电与保活”一致；类型检查通过，原有 59 项加新增 6 项 JVM 测试通过。
- 本轮改动见 DESIGN“状态优先的首页”。协议、端口、去重、落盘后 ACK、通知节流和自动复制条件均未改。
- 验证环境是云端 Linux，没有 Android SDK 和 Windows：Android 用 Robolectric android-all（API 36）做 javac 类型检查，R 为脚本生成的桩，83 项 JVM 测试通过，未运行 aapt2、d8、lint 或模拟器。Windows 用 `GOOS=windows` 完成 vet、测试编译和 GUI EXE 交叉编译；纯逻辑测试（通知策略、状态文案、Toast XML，共 13 项）抽到 Linux 包运行通过；含 WinRT/注册表的测试未运行。
- 待真机确认：首页在 360 dp 宽、200% 字体下的排版；状态色在深色主题下的对比度；Windows ScrollView 内分组的实际布局和托盘提示截断；Toast attribution 行的显示。

### 2026-10-01 / Codex / DONE — v0.7.6 双端预览发布及网站下载

- 用户明确回复“授权”，允许发布上一轮 UI、通知与空闲等待改动，并同步安装包、源码、清单和网站。只在唯一共享开发目录操作，Zcode 为 IDLE；未创建第二开发目录。
- Android versionCode 16 / versionName 0.7.6；Windows appVersion 0.7.6。SDK 构建、59 项 JVM、v2/v3 签名通过；APK 的代码和资源哈希与前序模拟器验证候选一致。Go test/vet/GUI 构建通过，PE subsystem=2。网页下载测试增加核对 Windows 源码版本，12 项通过；Worker typecheck 和 keep-vars/strict dry-run 通过。
- 功能源码提交 `7b728d723ef8cba8144d74f84e9779a8b4e4bd15`，远端标签 `v0.7.6` 已核对指向该提交。使用安全推送脚本原子推送 maintenance/shared-workspace 分支和标签；未动 main，未创建或合并 PR，原 PR #1/#2/#3 仍为 open。
- [v0.7.6 预览 Release](https://github.com/121103qwq/MsgDock/releases/tag/v0.7.6) 已公开，isDraft=false、isPrerelease=true。上传前核对草稿资产 digest，公开后 APK、EXE、源码 ZIP、清单四项匿名 GET 均为 200，实际字节 SHA-256 与本地及 GitHub digest 全部匹配。下载验证文件保留在 build/download-verification-v0.7.6/。
- APK 115870 字节，SHA-256 `C9E099BC7CEE9DD93EA55A0F86DD3E4093D73F9301A4D237B28EC64F9FD92226`；EXE 10535936 字节，SHA-256 `2B00BFB62C40639C249DEB4DBC593A71E85819AA4D4F1B38A20D9B59DE6B9719`。
- 源码 ZIP 从固定提交 git archive 生成，含 123 个文件，363472 字节；不含备份源码、构建输出、local.properties、依赖目录或凭据文件，暂存源码敏感模式扫描无命中。SHA-256 `4A26C082AFE738EBD4C46F05B8E8A5DC4F98D7B42DE11895BF94F7AF023BD79A`；清单 SHA-256 `3724904F9DA713A0C7CEDE3E33251988796061D0E073944D567FD7568892654D`。
- 网站部署 `1e3ef438-2086-4ca7-9bd4-17e9b5c2d017`，Wrangler 只上传修改的 /index.html，业务源码、配置、绑定、变量和 D1 未改。主域根页、/inbox、备用根页均 200，下载区及内联业务脚本逐字匹配；主备 health 200，匿名消息 401/no-store，微信 TXT 200 且内容未变。
- 本轮未重新启动模拟器、安装到真机、使用真实账号或写用户剪贴板。Release 与文档继续明确真实 SMS、HyperOS、后台保活、耗电量和 Toast 点击/剪贴板端到端待验；服务器推送未实现。发布验收只证明资产与网站送达，不替代这些运行验收。
- 发布需求—证据 audit 返回低置信度，未用其作为批准结论；主代理按实际构建、远端 API、下载哈希与网站 HTTP 证据逐项核对，没有重复调用以求通过。
- 源码包固定在功能提交，不因本条发布后记录而重打包或移动标签。后续文档提交只记交付结果。MainActivity.java.backup 原样保留且未跟踪，旧版本资产未覆盖。
- 本轮命令纠错：git grep 的 --cached 必须放在路径分隔符 -- 之前；首次参数位置错误未执行扫描，调整后已实际完成扫描再提交发布。

### 2026-10-01 / Codex / DONE — UI、通知与账号接收空闲等待（仅本地）

- 用户澄清“公号”是“降低功耗”；“发送”结合上下文按发送者与短信正文处理，不增加发送短信功能。继续复用现有 MsgDock 仓库和 maintenance/shared-workspace 分支，未新建远端仓库。
- Android：SDK 打包的 uses-sdk 改为插入 application 之前，修复兼容留白；新增 WindowLayout，由根容器统一处理系统栏、挖孔与键盘边距。主页面和教程页复用，键盘出现时滚动到输入焦点。未覆盖 Zcode 的主题或原有功能。
- 省电：ReceiverService 未登录、未启用账号接收或需重新认证时改为事件等待；设置与网络变化唤醒，并以版本计数防止丢失通知。原 SMS 广播上传、Outbox 重试和协议未改。开启账号接收后仍为 3 秒轮询；服务器推送尚未实现，不能将空闲等待宣传为完整推送或长期功耗验收。
- Windows：取消 urgent/long，使用普通短时静音 Toast；固定 Tag/Group 替换同一张卡片，10 秒内抑制重复横幅。历史补齐静默，近期明确验证码自动复制；原生失败才使用节流托盘兜底，手动复制成功不再新增气泡。复用现有通知注册、激活回调和落盘/ACK 语义，补充小型 WinRT 绑定，无新增依赖。
- 验证：最终 Go 全套 test、vet 通过，GUI 构建成功且 PE subsystem 为 2。原生 WinRT 属性测试通过；隔离 QA AppID 实际投递 3 次，通知中心保留 1 张卡片，测试注册与历史已清理。策略测试覆盖 20 条连续消息、历史/未来时间、失败重试、验证码识别和去重。真实客户端横幅、按钮激活和实际剪贴板写入尚未端到端验收，未覆盖用户剪贴板或启动其客户端。
- Android SDK 构建、59 项 JVM、v2/v3 签名通过，Gradle lint 未运行。独立 Android 14 MCP 确认窗口不再 letterbox；全屏、键盘焦点与收起恢复见 build/android-ui-2026-10-01T08-50-31-157Z/result.json。该首轮报告的横屏断言误判，原报告保留；真实大字体横屏及教程验证以 build/android-ui-2026-10-01T08-52-56-992Z/result.json 和 2400×1080 截图为准。
- 空闲观察：独立模拟器内开启接收但未登录，账号循环线程在 7 秒观察窗内的上下文切换计数不变（voluntary 3、nonvoluntary 1）。这只支持没有每 3 秒定时唤醒，不代表耗电量或真机后台验收。检查后通过 MCP 停止接收，恢复竖屏和 font_scale=1.0；崩溃日志为空。独立模拟器和 5038 ADB 已关闭，helper Status 的两项 PID 均为空，未操作其他设备。
- 候选仅位于 build/ui-notification-candidate/，未提升版本或覆盖正式 outputs。APK SHA-256：FC5A9AF9F6DD6A154C8878257863DF5F08C68B7E65F2158C5F21BEFFFAA6F0EE；Windows EXE：C33E4D07A0621D92EBC7597F5328DB017F23F768FFE983C8A22085881593945C。旧签名与包名保留。
- 需求—证据 audit 未判为整体完成：剪贴板交互只有实现/策略证据；真机、服务器推送和发布证据缺失，UI/省电摘要置信不足。主代理核对具体模拟器与原生测试记录，保留上述有限结论，不把本地通过当成真实设备或线上交付。
- 本轮没有提交、推送、创建 PR/Release、合并或部署。网站仍为 Android v0.7.5、Windows v0.7.0；发布须获得当前任务授权，并一并提供安装包、源码与 SHA-256 清单，更新对应网站链接和实际下载验哈希。未跟踪 MainActivity.java.backup 原样保留。
- 防重复错误：SDK-only manifest 必须先写 uses-sdk；MCP 旋转操作返回成功不等于画面已旋转，断言须等待实际宽高并查截图。Go 命令从 windows/ 执行，本机可用路径为 D:/DevTools/Scoop/apps/go123/1.23.12/bin/go.exe；rg 文件通配符用 -g，不将 windows/*.go 当作 Windows 路径传入。

### 2026-09-06 / Codex / DONE — 开发目录接入现有 Git 仓库

- 已现场确认远端为 `https://github.com/121103qwq/MsgDock.git`，默认分支 `main`；没有新建远端仓库。
- 在本目录初始化 `.git/` 并 fetch 现有分支/标签；本地工作分支为 `maintenance/shared-workspace`，基于 `origin/release/msgdock-v0.7.5` 的 `f23e2dd`，保留完整发布历史。仅 mixed reset 建立索引基线，没有 checkout 覆盖工作文件。
- 核对结果：除本轮 HANDOFF 记录外，全部已跟踪文件与该远端基线一致。现有未跟踪 `app/src/main/java/com/xgy/lansms/MainActivity.java.backup` 原样保留，不加入提交或源码发布包；local.properties、构建日志及 outputs 产物已被忽略。
- 本轮未提交、推送、合并 PR、创建 Release 或部署网站；发布快照未修改。今后仍只在本目录开发，开工前刷新认领。

### 2026-09-06 / Codex / DONE — Android v0.7.5 后台运行教程

- 用户要求下个版本汇总各品牌电池优化、最近任务小锁、自启动教学，并继续同步更新网站下载。
- 已实现独立原生离线教程页与 10 组品牌内容、自动品牌推荐/手动切换、系统设置按钮和准确的标准电池状态；官方资料与旧版/通用路径边界记录在 `docs/background-guide-sources.md`。协议、接收服务、Windows、Cloudflare 代码/配置未改。
- SDK 构建一次成功，59 JVM 通过，签名 v2/v3 通过；APK v0.7.5/code15/SHA-256 `C30FEB52E80FD01EAE5973987A35E5069753A7029D82FB78D4978A58AD938F5D`。网页 12 项、typecheck、dry-run 通过。
- 实际 MCP 确认独立模拟器 `MsgDock_Codex_API34` / `emulator-5680` / Android 14，使用 5038。自动化前半段验证 0.7.4→0.7.5 保留历史、自动识别和 10 组完整内容；结果保存在 `build/android-ui-2026-09-06T08-20-24-654Z/`。
- 大字体横屏的自动滑动长度超过滚动区域导致脚本失败；没有改应用，测试工具改用 UI 区域内手势。随后直接 MCP 已滑到底部并打开华为对应的两个官方来源选项，确认 1.3 字体与旋转保留选择；两个快捷按钮实际打开 Android Settings 的 App info / Battery optimization 并成功返回，页面返回首页。标准电池白名单前后相同、crash buffer 为空、font_scale 恢复 1.0。
- 原始脚本 `passed:false` 如实保留；后续交互完成剩余验证，没有重复重跑已通过的 APK/品牌测试。MCP 原生 save_screenshot 在 Windows 路径上拒绝保存，未改插件；已有脚本截图和直接 UI/截图输出作为证据。
- 已交付：代码提交 `ed3a2d93d9b948418e4367d64575f27cf34e184e`，标签 `v0.7.5` 的远端目标已核对；功能分支 `release/msgdock-v0.7.5`、[PR #3](https://github.com/121103qwq/MsgDock/pull/3) 保持未合并。旧 PR 未改动。
- [v0.7.5 预览 Release](https://github.com/121103qwq/MsgDock/releases/tag/v0.7.5) 发布 APK、源码 ZIP、SHA-256 清单三个文件；草稿资产先核对 GitHub digest，公开后无登录实际 GET 三个文件均 200 且 SHA-256 等于本地。源码 ZIP 148 项，不含构建输出、凭据或备份文件；SHA-256 `B62C96438E9C19C2D6E75D53269089ED2D1E36DC380721621A6869C3E3C763ED`。
- 网站已部署 `07c86e6c-741d-478b-8afb-668183edd0b0`，仅上传修改的 `/index.html`，保留配置/绑定/变量。主域根页、`/inbox`、备用 workers.dev 根页均 200，下载区及原有脚本逐字匹配本地；`/health` 200、匿名消息 API 401/no-store，旧 Windows v0.7.0 下载 HEAD 200。
- 清理完成：本轮模拟器与独立 ADB 已关闭，最终 helper Status 的 AdbPID/EmulatorPID 均为空；未开启浏览器或本地网站服务。没有新的真机验收结论。后续如修改教程，应先核对厂商当前资料，不把通用方法写成全机型准确路径。

### 2026-09-06 / Codex / DONE — Android 本机收件箱与对应网站下载

- 用户追加要求：每个新版本构建同步更新网站对应下载链接。已写入本项目 AGENTS；Android 与 Windows 独立标版本，不发布缺失或不匹配的链接。
- Android v0.7.4/code14：首页本机收件箱显示未登录 LAN/配对云端历史及当前账号历史，先隔离账号再按 deliveryId 去重，展示最近 200 条、保留底层历史；列表/详情标明来源时间，复制前复核账号。
- SDK 编译、56 项 JVM、APK v2/v3 签名通过。MCP 首次在安装前因未登录设备没有账号偏好文件而中止；修正测试前置假设后，v0.7.3 覆盖升级保留历史、真实 LAN HTTP 重发去重、全文/验证码实际粘贴、进程重开、本地账号 A/B/退出隔离共 5 项全部通过。报告 build/android-ui-2026-09-06T06-27-53-094Z/result.json；crash-log.txt 为空，合成账号数据已恢复，截图已人工检查。
- 网站 12 项回归、Worker typecheck、keep-vars/strict dry-run 通过；未改 Worker 逻辑、配置、绑定、D1 schema 或 Windows。
- 产物：outputs/MsgDock-v0.7.4/MsgDock-Android-v0.7.4-debug.apk，103496 字节；SHA-256 32B07514D26C74FDA406209D7941C12A498441A0CA9A32CAD040E428A804684F。与旧版使用同一调试证书。
- 消融审查：沿用一个 JSONL，只增加有上限的去重读取与来源格式化；无新数据库、后台服务、生产依赖或通用框架。测试复用已有 MCP 脚本。
- 清理：临时转发已移除，独立模拟器与 5038 ADB 已关闭；模拟器退出时设备注销有短暂滞后，待列表清空后仅重试专用 ADB 清理。
- 发布：功能分支 release/msgdock-v0.7.4 与标签 v0.7.4 的代码提交 cb1af52e89d23c7af621f21bb5477a11b264559e；PR #2 保持未合并。预览 Release https://github.com/121103qwq/MsgDock/releases/tag/v0.7.4 只新增 Android APK、该提交的源码 ZIP 和 SHA256SUMS。
- 资产实证：三个新资产均匿名 GET 成功，实际下载字节 SHA-256 与本地/GitHub digest 一致。源码 ZIP 327648 字节、141 项且无构建缓存/凭据/成品/备份源码；SHA-256 26E888E08FB088EB61B4E891FAA16C64256E6523AABE0283DF0BC4410A03D441。Windows v0.7.0 原链接 HEAD 200、原校验文件 GET 200，未重建或重传 EXE。
- 网站：keep-vars/strict 部署 cb054c5e-0fe0-4d1a-8e87-241c5c2064c3，仅 /index.html 一项静态资产变化。主域名 / 与 /inbox、备用首页均 200 且下载区/内联脚本与本地一致；主备 health 200、未登录 messages 401/no-store、微信 TXT 200/内容未变。未更改绑定、域名、Secret 或 D1 schema。
- 边界：真实 SMS、锁屏、HyperOS、后台保活和真机主备切换未验证；未进行真实账号端到端收发，Gradle lint 仍受 B-005 限制。该轮交付完成不表示 B-001 真机门槛已通过。

### 2026-09-06 / Codex / DONE — Android 权限和真实接收状态

- 目标：按用户要求逐项改进并在 Android 设备运行验收。本轮未检测到 USB 调试真机，使用 Codex 独立模拟器；仍保留真机验收门槛，不将本目标标为整体完成。
- 修改：MainActivity 权限回调及 Activity 重建恢复、拒绝通知仍接收、一次有时限 LAN 扫描/防重复/销毁取消/失败提示；ReceiverService 显示实际启动与监听结果。保留 SMS 广播、6 位配对、LAN 协议和独立云循环；未改 Worker/Windows。
- 三轮验证：SDK 完整编译 + 53 项 JVM + APK v2/v3 签名；Android 14 MCP 权限/界面及实际 LAN HTTP 7 项；端口占用、故障解除、进程重开及权限弹窗旋转 4 项。后两轮通过报告：build/android-ui-2026-09-05T17-40-08-418Z/result.json、build/android-ui-2026-09-05T17-45-48-124Z/result.json，截图和空崩溃日志在同目录。
- 具体结果：拒绝通知时仍鉴权接收并持久保存，同 UUID 重发只保存一次；故意占用端口复现 EADDRINUSE，界面报 LAN 失败；释放后停止/启动可恢复。显式重开进程保留启用设置与历史；不是强行停止后自动复活，也不是整机重启验收。
- 产物：outputs/MsgDock-v0.7.3/MsgDock-Android-v0.7.3-debug.apk；SHA-256 FF9C7925A144AF52F52CCAAEEBAB715A699F2155C31B11971FAE81117D56BBC4。versionCode 13，包名/调试证书与旧版相同。
- 消融审查：保留一个待授权动作编号和一个实时服务状态，分别保证权限返回后正确继续和避免误报；删除每次扫描永久存活的 Executor，使用单次有时限线程。无新 Manager、服务、配置文件、生产依赖或外部测试框架。
- 清理：本轮临时 ADB forward/reverse 已移除，独立模拟器及 5038 ADB 已关闭，原 Oppo Connect 5037/PID 14480 未动。首次停止时设备注销记录短暂滞后，先核对模拟器已退出且列表清空，再仅重试专用 ADB 清理，没有强制终止其他进程。
- 边界：未登录或创建真实账号、未发送真实短信、未安装到真机、未推送 GitHub/发布 Release/部署。Gradle lint 仍受 B-005 主机故障限制。下一步需连接测试手机并允许 USB 调试，覆盖安装后验收首次授权、真实收发、锁屏及重开行为。

### 2026-09-05 / Codex / DONE — Web 优化上线

- 用户在告知待部署后回复“继续”，授权发布。现场确认旧生产版本 4016d74e-15c9-49c6-8a0c-3ef4d5af04a5；使用现有 Wrangler 4.125.0，keep-vars/strict 发布，未更改配置、Secret 或 D1 schema。
- 新生产版本 40fae349-ca91-4296-b86c-e68d0c57955d。部署输出只新增/修改 /index.html 一个静态文件，保留微信 TXT、旧 Relay、主/备用域名及所有绑定。
- 两轮必要验证：部署前 11 项 Web 回归、TypeScript 与 dry-run 通过；部署后 / 和 /inbox 返回 200，线上完整内联脚本与本地一致，HTML public/must-revalidate/max-age=0；两个域名 health 200；未登录 messages 401/no-store；微信 TXT 200 且与本地一致。
- 所有本轮命令进程正常退出，无新增服务器/浏览器标签。未改客户端下载 v0.7.0、未重新构建 APK/EXE、未推送 GitHub。实机浏览器体验/跨网络性能仍需用户验收，不把 HTTP 成功当成性能评分。

### 2026-09-05 / Codex / DONE — Web 低复杂度性能优化（本地）

- 根据用户粘贴建议核对现状：域名已绑定同源 Web/API；首页本轮 HTTP 基线 200、Brotli、CF-Cache-Status HIT、public/max-age=0/must-revalidate、alt-svc h3，动态 JSON 原有 no-store 已具备。保留这些有效配置，不重复加缓存层。
- 修改 web-ui/index.html：站内导航不重载；恢复/最近页并行；无消息不重绘，有消息保留展开项；单次请求 15 秒超时；完成后调度下一轮，错误退避到 60 秒；隐藏/离线暂停，恢复事件立即补齐。分页内存上限 200，全部分页成功才提交游标，旧账号响应隔离；API 客户端显式 no-store。
- 新增 tools/test-web-ui.mjs（Node 自带 test/vm，无依赖），11 项通过：缓存选项、空轮询不重绘、并行恢复去重、304 条缺口分页/200 条保留上限、账号隔离、无并发堆积、退避上限、隐藏/离线恢复、请求超时、坏 JSON 不推进游标、401 和站内导航。
- 两轮必要验证：线上只读 HTTP 基线 + 本地行为测试。IPv4 DNS 0.015582s / TCP 0.216848s / TLS 0.513620s / TTFB 0.772442s / total 0.772545s，收到 7378 字节；IPv6 curl 6 无法解析，不能比较速度。使用 --noproxy，但未切换用户代理/TUN，不能声称纯大陆直连。
- 新 HTML 25480 字节，本地 Brotli 7014 字节；本地压缩条件与线上不同，不作为线上优化百分比。Chrome DevTools MCP 不可用，按 web-perf 技能停止浏览器性能 trace，不报告 LCP/INP/CLS 分数。
- 消融审查：保留现有单文件/短 HTTPS API，不增加哈希资源构建、长连接、缓存正文、数据库表、第三方依赖或优选 IP；共享超时和单一轮询延迟状态分别解决卡死与请求堆积，需保留。Android/Windows/Worker、TXT 验证文件和下载版本未改。
- 同步 web-ui/README.md 和 DESIGN.md。按 AGENTS 发布边界，本轮未部署 Worker、未推送 GitHub；用户授权发布后部署此静态页面即可，无 D1 migration/Secret/客户端重打包需求。

### 2026-09-05 / Codex / DONE — Android 固定主备 Relay

- 主地址 msgdock.dpdns.org、备用 xgy-sms-relay.xgy2021sh.workers.dev 固定内置；移除地址编辑和保存按钮。每次优先主地址，旧保存链路也采用主备顺序，原凭据/消息 ID/历史不修改。
- RelayHttp 复用账号、加密 Relay 和设备备份原有 HTTP 请求。确定的 DNS/TCP/TLS 建连失败可走备用；发送后仅安全读取、去重消息上传和 ACK 能重放。未知写入结果、认证失败、冲突、限流和重定向不触发重放；历史自定义来源不跨域发送凭据。LAN 与原有 Outbox/轮询调度未改。
- 文件：app/build.gradle；RelayHttp.java；RelayHttpTest.java；CloudRelay.java；AccountApi.java；DeviceBackupManager.java；MainActivity.java；activity_main.xml；README.md；DESIGN.md；HANDOFF.md。
- 两轮必要验证：最终 SDK 编译、53 项 JVM 测试、APK v2/v3 签名通过；aapt 确认包名 com.xgy.lansms、v0.7.2/code12；主备 /health 均返回 200。未运行 Gradle lint；ADB 无连接设备，手机覆盖安装/锁屏/真实断网主备切换仍待验收。
- APK：outputs/MsgDock-v0.7.2/MsgDock-Android-v0.7.2-debug.apk；SHA-256 D16357439C18EFB0463F3F1F70378940789E855623E4C9A78E3D0049034FFBB2。调试证书 SHA-256 8b72e245377b56f06af95753d8fd396a880bf608a3161b6c175e443d94dbf82a 与旧版一致。
- 消融审查：合并三份重复 HTTP 实现，保留一个主备传输实现及无状态测试入口；无探测线程、缓存、额外后台服务或依赖。未修改 Windows/Worker，未推送 GitHub/发布 Release/安装 APK；网站下载仍是 v0.7.0。

### 2026-09-05 18:00 / Codex / DONE — Android 账号接收、注册修复、微信验证

- Android 新增可选账号接收与本机历史/复制，独立短 HTTPS 轮询、持久游标、LAN/云通知去重与账号隔离；设备撤销等待重新登录。旧 LAN、SMS 广播和配对接收保留。
- APK：outputs/MsgDock-v0.7.1/MsgDock-Android-v0.7.1-debug.apk；SHA-256 1AC6B21C8CA8EEE2467E012D434AF3E7EECE8166D1C95BAE6328D1A3893A0BF7。versionCode 11，包名及调试证书与旧版一致。40 项 JVM 测试、SDK 编译和签名验证通过；ADB 无真机，未执行覆盖安装和锁屏验收。
- 复现网页 HTTP 表单可用但 Origin 被拒；HTTPS 同源入口也误受域名白名单影响。Worker 先处理静态页面、HTTP 308 HTTPS、拒绝 HTTP POST、允许 HTTPS 精确同源，陌生/null Origin 继续拒绝。
- Worker typecheck、13 项测试及 dry-run 通过；线上同源注册、Secure/HttpOnly Cookie /me、注销通过；HTTP 注册页 308，主域名/旧 workers.dev 预检 204，外部来源 403，两个 health 200。仅新增无短信内容的合成测试账号，测试 session 已注销。
- 按用户截图发布 web-ui/26a982afdfe8ade2696ce7d9359ea892.txt；HTTPS 返回 200 text/plain 且内容一致。用户需要在微信页面点击开始验证，不能把文件可访问当成微信审核通过。
- 消融审查：复用旧前台服务、JobScheduler、收件账本和 Worker；不增加数据库表/服务/生产依赖。仅测试使用 org.json；SDK 构建脚本保留用于已知 Gradle 主机故障。
- 本次没有推送 GitHub、修改已有 Release 或更新网站 APK 下载版本；当前网站下载仍是 v0.7.0。本地新源码与 APK 待用户实机测试/后续授权发布。

### 2026-09-05 / Codex / DONE — GitHub 与网页下载

- 新建公开仓库 https://github.com/121103qwq/MsgDock ，旧私有 codex-cloud-test 未修改。
- 源码提交 `06a967e0f69ff6354796d629db6ccc75d71a3952` 位于 `release/msgdock-v0.7.0`；PR #1 未合并。预览 Release `v0.7.0` 标签和源码 ZIP 均指向此提交。
- Release 只包含 Android debug APK、Windows x64 EXE、源码 ZIP 和 SHA256SUMS；四个资产的 GitHub SHA-256 与本地一致，匿名下载 HEAD 均为 200。
- 网站未登录/登录后共用静态下载区，不增加下载 API、状态或依赖；Worker 仅静态 index.html 发生变化。
- 验证：源码敏感文件排除、客户端哈希与版本、网页 JS 语法、Wrangler dry-run、线上 HTML（扣除 Cloudflare 自动注入统计脚本后）一致；主页和两条 health 地址 200，未登录 messages 401。
- 更新旧手动构建工作流，Windows 测试移到 windows-latest；本轮没有运行 GitHub Actions，不宣称 CI 通过。保留旧代码现有尾随空白，未做无关格式化。
- 消融审查：直接复用 GitHub Release 和现有静态网页，无新服务、路由或下载状态管理。
- 遗留：真实手机锁屏/HyperOS/SMS 与 Toast 点击仍待验收；Android 为调试签名，Windows 未签名。
- 共享开发仍使用本目录；`../work/msgdock-publish` 是发布快照 checkout，不作为第二开发工作区。安装包未因这次网页更新而重新构建。

### 2026-09-04 / Codex / DONE

- 建立独立共享工作区及协作规则。
- 从 v0.6.1 干净源码包提取，没有复制构建缓存和成品。
- 新增 `AGENTS.md`、`DESIGN.md`、`BUG.md`、`HANDOFF.md`、`START_PROMPT.md` 和 `.gitignore`。
- 本轮只整理工作区和长期上下文，没有重新构建应用或改变功能。

后续记录格式：

```text
### YYYY-MM-DD HH:mm / Agent / DONE|BLOCKED
- 目标：
- 修改：
- 验证：
- 遗留：
```
### 2026-09-04 21:48 / Zcode / DONE

- **目标**：为 Xgy LAN SMS 创建现代化 UI 并构建新版本
- **修改**：
  - Android：创建 Material Design 3 深色主题资源（colors.xml, dimens.xml, styles.xml，青色系配色）
  - Android：创建 XML layout 文件（activity_main.xml, item_cloud_link.xml, item_target.xml）
  - Android：重写 MainActivity.java 使用 XML 布局替代纯代码构建，保持所有功能逻辑 100% 向后兼容
  - Android：更新 AndroidManifest.xml 使用新的 AppTheme
  - Android：修复中文引号导致的编译错误（activity_main.xml 和 MainActivity.java）
  - Windows：成功构建新 UI 版本 EXE（保持功能不变）
  - Web：创建单文件 Web 管理面板（web-ui/index.html）用于状态查看和设备管理
  - Web：提供响应式设计、实时状态展示（演示数据）、部署文档（web-ui/README.md）
  - 文档：更新 DESIGN.md 记录 UI 设计原则和实现细节
  - 配置：创建 local.properties 配置 Android SDK 路径
- **验证**：
  - ✅ Android：成功编译 `app-debug.apk`（90KB，versionCode 9）
  - ✅ Windows：成功构建 `XgyLanSmsReceiver-v0.5.0-new-ui.exe`（11MB）
  - ✅ Windows：所有测试通过（`go test ./...`）
  - ⚠️  未在真机上测试 UI 渲染和交互
- **构建产物**：
  - `XgyLanSms-v0.6.1-new-ui.apk` (90KB)
    - SHA-256: `eb290e563d4c876d967ac89ccff5a11175613ee939e76d5928c5c302066ab409`
  - `XgyLanSmsReceiver-v0.5.0-new-ui.exe` (11MB)
    - SHA-256: `9e5458ba928b85e4b4423eb61deac6d00592319591272407a60b3ae21186326f`
- **遗留**：
  - P0：需要在 Android 真机上验证新 UI 的渲染、布局、交互和深色主题对比度
  - P1：Web 面板当前为静态演示，需实现后端 API（/api/status, /api/devices, /api/messages）
  - P2：Web 面板未包含认证机制，仅适合可信网络
  - P2：考虑未来为 Windows 端添加内置 Web 服务器以提供管理面板

### 2026-09-05 15:30 / Codex / BLOCKED

- 目标：实现 MsgDock 账号/D1/Web 模式、Android 可靠 Outbox、Windows 账号轮询与本地交付。
- 修改：新增 `/api/v1` 账号、设备和消息 API 与 D1 migration；Web 改为真实登录收件箱；Android
  增加先落盘账号 Outbox、独立三路转发、指定退避与网络切换重试；Windows 增加账号登录、3 秒
  增量补齐，并保持无控制台、托盘、Toast、历史和旧协议。
- 验证：Worker typecheck、10 项 Vitest 与 Wrangler dry-run 通过；Windows test/vet 和 GUI PE
  subsystem 验证通过；Android 使用 SDK 工具完整编译，30 项 JUnit 与 APK v2/v3 签名校验通过。
- 产物：`outputs/MsgDock-v0.7.0/` 下 Android APK、Windows EXE、源码 ZIP 与 SHA-256 清单。
- 遗留：当前 Cloudflare 页面未登录，Wrangler token 缺少 D1/route 权限；用户登录后继续创建 D1、
  应用 migration、部署 `msgdock.dpdns.org` 并做线上与真机闭环验收。

### 2026-09-05 / Codex / DONE

- 用户登录并授权 Wrangler；创建 `msgdock` D1、应用 0001 migration、部署同源 Web/API 和自定义域名。
- 线上发现并修复 PBKDF2 迭代上限：100,000 次 + 用途隔离 HMAC；旧 Secret、DO 与云链路未替换。
- 本地 typecheck/10 项测试通过，线上 16 项账号/API 验收通过；旧云正在使用的拉取请求仍返回 200。
- 新增 `DELIVERY-v0.7.0.md` 汇总架构、文件、构建、API、测试和消融审查；更新源码 ZIP/校验清单。
- 待用户真机检查锁屏 SMS、网络切换、Toast 操作和开机自启。本轮 UI 读取超时，未把 HTTP 成功等同于视觉验收。
