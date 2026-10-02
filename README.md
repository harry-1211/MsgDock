# MsgDock

MsgDock 把 Android 收到的传统 SMS 同步到局域网 Windows、互联网 Windows 和 Web 收件箱。
本版本是在原 Xgy LAN SMS 上的兼容升级：已有短信监听、锁屏前台服务、LAN 58123/58124、
6 位配对和旧加密云链路全部保留。

## 使用与下载

- 网页收件箱：[msgdock.dpdns.org](https://msgdock.dpdns.org)
- [Android v0.7.6 预览版](https://github.com/121103qwq/MsgDock/releases/download/v0.7.6/MsgDock-Android-v0.7.6-debug.apk)：沿用旧调试签名的 APK。
- [Windows v0.7.6 预览版](https://github.com/121103qwq/MsgDock/releases/download/v0.7.6/MsgDock-Windows-v0.7.6.exe)：x64 单 EXE。
- [版本说明、完整源码和 SHA-256 清单](https://github.com/121103qwq/MsgDock/releases/tag/v0.7.6)。
- [公开源码与开发记录](https://github.com/121103qwq/MsgDock)。开发分支为 `maintenance/shared-workspace`，发布源码由标签 `v0.7.6` 固定，实机验收完成前不自动合并。

这是预览版：Android 为调试签名，Windows 未做代码签名，锁屏/HyperOS 和真实短信端到端仍待实机验收。升级前请保留旧版备份；GitHub 下载在部分网络下可能较慢。

## v0.7.7 候选（未发布）

本节描述分支 `claude/ui-status-first` 上的改动，尚未发布安装包，也未经真机验收。

- 两端用同一套五种状态说明同步情况：同步正常、正在同步、同步延迟、同步中断、需要处理。规则见 `DESIGN.md` 的“同步状态模型”。
- Android 首页依次为状态卡、待处理事项、最近 5 条短信、接收端、账号和高级设置。账号和高级设置默认折叠，待处理事项每条带一个处理按钮。
- Android 通知在识别到明确验证码时增加“复制验证码”按钮；锁屏隐藏敏感内容时只显示“收到 1 条短信”。
- Windows 托盘图标按状态显示彩色圆点，主窗口分“概览”和“设置”两页，最近短信改为表格，双击复制验证码。
- Windows 通知按发件人分开保留，同一发件人只保留最新一张；10 秒内不重复弹横幅的规则不变。新增隐私预览三档，默认显示完整内容。
- 界面统一使用“局域网配对码”和“云配对码”两个名称。账号同步不是端到端加密，界面说明已如实标注。
- 同步时序、LAN v1 协议、端到端加密和省电策略没有因界面改动而变化。

## v0.7.6 改进（2026-10-01）

- Android：修复 SDK 兜底打包导致的兼容留白，统一系统栏、挖孔和键盘避让；未启用账号接收时改为事件等待。
- Windows：取消紧急通知，连续短信更新同一张通知卡片，10 秒内不重复弹横幅。显示发送者和正文，保留复制操作；近期且有明确验证码语义的短信自动复制验证码。历史补齐不弹窗、不自动复制。
- Android 独立模拟器布局检查和 Windows 原生通知 API 检查已通过。真机锁屏、真实短信、Windows 通知按钮点击和实际剪贴板端到端仍待验收。
- Android 与 Windows 本次均更新为 v0.7.6，旧版本资产保留。发送手机本来就是收到短信即上传；接收端仍使用轮询，尚未接入服务器推送。验证范围见 `DELIVERY-v0.7.6.md`。

## 架构

```text
Android SMS_RECEIVED
  ├─ Account Outbox（先落盘）─ HTTPS ─ Worker + D1 ─ Web / Windows 轮询
  ├─ LAN HTTP（低延迟）──────────────────────── Windows / Android
  └─ 旧 E2EE Outbox ─ /v1/* Durable Object ─── 已配对 Windows / Android
```

- `app/`：Android 发送/接收客户端。
- `windows/`：Windows x64 原生 GUI/托盘客户端。
- `cloudflare/`：同一个 Worker 中的账号 API、D1 和旧 Relay。
- `web-ui/`：由 Worker 同源提供的无框架响应式网页。

## 两种互联网同步模式

账号模式是新的 Web/Windows 历史链路。短信正文经 HTTPS 上传并保存在 D1，登录后可以直接
在浏览器查看，因此它不是端到端加密。数据库只保存密码哈希、session token 哈希和 device
token 哈希，不保存明文凭据。

旧 `/v1/*` 配对 Relay 继续使用 P-256 ECDH、HKDF-SHA256 和 AES-256-GCM，Cloudflare 只看到
密文。旧链路不会因升级到 v0.7.0 失效，也不会与账号 token 混用。

## Android v0.7.6

- 首页新增“后台运行教程”：按品牌推荐，也能手动切换；离线说明电池优化、最近任务小锁、自启动三项设置。小米包含长按卡片点小锁与新版手机管家入口，华为/荣耀说明“不允许电池优化”的含义。
- 覆盖小米/Redmi/POCO、华为、荣耀、OPPO/一加、realme、vivo/iQOO、三星、华硕/ROG、Google/原生 Android，并为其他品牌提供明确标注的通用排查；[来源与版本范围](docs/background-guide-sources.md)可查。
- 教程提供系统电池列表和应用信息快捷入口，不自动改设置；只读取系统可确认的电池优化状态。后台小锁和自启动需自己检查，不能保证应用永不被系统结束。

- 首页“本机收件箱”无需登录即可查看 LAN / 配对云端已保存短信；登录后也显示当前账号历史。不同账号隔离，多路副本合并显示，支持全文/验证码复制。
- 列表按最近接收排列，显示设备、来源和时间，最多展示 200 条，旧历史不删除。
- 在账号区勾选“接收账号短信（无需配对）”，登录与发送手机相同的账号即可接收；接收端无需短信权限。“立即收取”可手动刷新，后台会分页补齐。
- 扫描等待附近设备授权并防止重复启动；接收状态反映实际监听结果。通知关闭仍保存历史，端口占用会明确报错。
- 主 Relay 固定为 `https://msgdock.dpdns.org`，备用为 `https://xgy-sms-relay.xgy2021sh.workers.dev`；可安全重试的网络故障自动尝试备用，LAN 独立运行。注册/登录等结果不明的写入不自动重复提交。
- APK：`outputs/MsgDock-v0.7.6/MsgDock-Android-v0.7.6-debug.apk`，versionCode 16，沿用旧调试签名。
- 新版验证范围和产物见 `DELIVERY-v0.7.6.md`。v0.7.4 已验证的收件箱、LAN 去重和复制逻辑本轮未修改。
- SDK 本机构建兜底：`./tools/build-android-local.ps1`，使用已有 SDK/JDK/测试依赖缓存。Gradle lint 因已记录的主机故障未运行；真实 SMS、锁屏、HyperOS 和后台保活仍待真机验收。

- 收到 SMS 后，若已登录账号，先把消息和唯一 `client_message_id` 写入本地 Account Outbox。
- LAN、账号云和旧加密云三条路径独立；任意一条失败不阻止另外两条。
- 账号上传失败按 `2s → 5s → 15s → 30s → 60s → 5min` 重试并复用原 ID。
- 进程存活时使用精确计时器；进程被系统回收后由持久 `JobScheduler` 兜底。
- Wi-Fi/移动网络恢复或切换时主动触发重试。
- 保留 `SMS_RECEIVED`、锁屏前台服务、开机/更新恢复、Android 云接收和旧设备备份。
- 增加用户名/邮箱登录与注册；登录后服务器为本机签发独立长期 device token。

Android 系统“强行停止”仍是硬边界：强行停止后必须手动打开一次 App。HyperOS 还应允许
自启动、电池无限制，并避免系统网络短信功能截断真实 SMS 广播。

## Windows v0.7.6

- 单 EXE、Windows GUI subsystem、无 CMD 黑框、不自动打开浏览器。
- 托盘驻留，可打开原生状态/设置窗口并正常退出。
- 用户名/邮箱登录、设备注册和设备移除。
- 每 3 秒调用 `GET /api/v1/messages?after=<last_seq>`；断网后从持久游标继续补齐。
- 短信先写入 `%APPDATA%\XgyLanSms\history.jsonl` 和待通知账本，再提交 Windows Toast。
- Toast 显示发送者与正文，并提供“复制验证码”“复制全文”；取消紧急通知，连续消息更新同一卡片。近期明确验证码自动复制；原生 API 失败时才使用托盘兜底。
- 原生 Toast 或持久化失败时不推进 `last_seq`，下次轮询继续处理。
- 可选开机自启使用 `--tray`，保留旧配置目录以便无损升级。

内部 Windows 通知 AppID、配置目录、单实例名和启动项名继续沿用旧值，这是为了保留现有
Windows 通知授权、历史和开机启动设置；用户看到的产品名已经是 MsgDock。

## Web

生产地址：`https://msgdock.dpdns.org`（2026-09-05 已部署，HTTPS API 验收通过）

页面包括：

- `/login`
- `/register`
- `/inbox`
- `/devices`
- `/settings`

Web 使用 HttpOnly session Cookie，不把 token 暴露给前端 JavaScript。收件箱每 3 秒增量请求，
按账号保存 `last_seq`；重开或长时间断网后会连续分页补齐全部缺口，再只保留最近 200 条用于
显示。支持深色/浅色自适应、展开全文、复制正文和复制 4～8 位验证码。

## Account API

| 方法 | 路径 | 凭据 | 用途 |
|---|---|---|---|
| POST | `/api/v1/auth/register` | 无 | 注册并创建 session |
| POST | `/api/v1/auth/login` | 无 | 用户名或邮箱登录 |
| POST | `/api/v1/auth/logout` | session | 注销当前 session |
| GET | `/api/v1/me` | session | 当前账号 |
| POST | `/api/v1/devices` | session | 注册设备并一次性返回 device token |
| GET | `/api/v1/devices` | session | 列出自己的设备 |
| DELETE | `/api/v1/devices/:id` | session | 撤销自己的设备 |
| POST | `/api/v1/messages` | device token | 幂等上传短信 |
| GET | `/api/v1/messages?after=<seq>&limit=<n>` | session/device token | 增量获取自己的短信 |

原生客户端登录/注册时发送 `X-MsgDock-Client: native`，JSON 才会包含 `session_token`；浏览器
只得到 Cookie。所有消息和设备 SQL 都从已验证 token 得到 `user_id`，请求不能指定别人的用户 ID。

D1 表和索引见 `cloudflare/migrations/0001_accounts.sql`。

## 第一次使用

1. 安装 `outputs/MsgDock-v0.7.6/MsgDock-Android-v0.7.6-debug.apk`，按使用功能授予权限；打开首页“后台运行教程”，检查本机的电池、后台小锁与自启动设置。
2. 退出旧 Windows 客户端后，运行 `outputs/MsgDock-v0.7.6/MsgDock-Windows-v0.7.6.exe`；首次防火墙提示只允许专用网络。旧配置和历史保留。
3. Android 和 Windows/Web 使用同一个账号登录。
4. Android 收到真实 SMS 后会先入队，再分别走 LAN 和互联网路径。
5. 旧 LAN 扫描、6 位码和旧云配对仍可继续使用，不要求重新配对。

远端没有内置测试账号。部署完成后可直接在 `/register` 创建自己的账号；密码最少 8 个字符。

## 本地构建

Android（常规构建）：

```powershell
.\gradlew.bat --no-daemon :app:testDebugUnitTest :app:lintDebug :app:assembleDebug
```

Windows：

```powershell
Set-Location windows
go test ./...
go vet ./...
go build -buildvcs=false -trimpath -ldflags="-s -w -H=windowsgui" -o ..\outputs\MsgDock-v0.7.6\MsgDock-Windows-v0.7.6.exe .
```

Cloudflare：

```powershell
Set-Location cloudflare
npm ci
npm run typecheck
npm test
npx wrangler deploy --dry-run
```

完整 D1 和域名部署顺序见 `CLOUDFLARE_DEPLOYMENT.md`。

## 当前验证

- Cloudflare：TypeScript typecheck 和 Wrangler dry-run 通过；Vitest `10/10` 通过，覆盖账号隔离、
  IDOR、token 用途隔离、消息去重、Cookie 属性及 205 条消息分页恢复。
- Windows：`go test ./...` 和 `go vet ./...` 通过；单 EXE 构建成功，PE subsystem 已读取验证为
  `IMAGE_SUBSYSTEM_WINDOWS_GUI (2)`。
- Android：本机 Gradle 因 Java NIO loopback 环境错误无法启动 daemon，使用已有 Android SDK
  本机构建兜底。各版验证与签名信息见对应 `DELIVERY` 文档；不把 SDK 编译当作 Gradle lint 通过。
- 当前没有连接 ADB 真机；锁屏、HyperOS、真实 SMS、Toast 点击和三端线上闭环仍需实机验收。

## 安全与兼容边界

- 公开 API 只允许 HTTPS；LAN v1 因局域网兼容继续使用 HTTP。
- 密码使用随机 salt 的 PBKDF2-SHA256（生产上限 100,000 次），再做服务端 Secret 保护的
  用途隔离 HMAC；token 使用 256-bit 安全随机值，D1 只保存 SHA-256。
- Web Cookie 为 `HttpOnly; Secure; SameSite=Lax`，登录/注册按来源 IP 做基础限速。
- 只同步传统 SMS；RCS/网络短信不会触发 Android `SMS_RECEIVED`。
- APK 使用测试签名，EXE 未做商业代码签名；系统可能提示未知来源/发布者。
- 覆盖安装必须保持 `applicationId` 和签名一致，否则 Android 会视为另一应用，旧本机身份也无法继承。

## v0.7.0 本地交付物

| 文件 | SHA-256 |
|---|---|
| `outputs/MsgDock-v0.7.0/MsgDock-Android-v0.7.0-debug.apk` | `7210A3C9BF94EDA74C51903E950E150E830D9CCA089AA7516FB2C5D3F2645502` |
| `outputs/MsgDock-v0.7.0/MsgDock-Windows-v0.7.0.exe` | `41B9AB7E0984B7992C90100028BE3DE1D74FA5BA27C12D1BF03334A642188BA4` |

源码 ZIP 和完整校验清单与上述文件放在同一目录。D1 migration 和 Worker 已部署完成，线上
16 项账号/API 验收通过；真机锁屏与 Windows Toast 交互仍需人工测试。详见 `DELIVERY-v0.7.0.md`。
