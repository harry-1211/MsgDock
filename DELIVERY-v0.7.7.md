# MsgDock v0.7.7 预览版（候选，未发布）

本文件记录 v0.7.7 的候选产物。Release 尚未创建，网站下载区没有改动。真机验收和发布后，再在本文件补记发布核验。

## 本次更新

- 两端用同一套五种状态说明同步情况：同步正常、正在同步、同步延迟、同步中断、需要处理。规则见 `DESIGN.md` 的“同步状态模型”。
- Android 首页依次为状态卡、待处理事项、最近 5 条短信、接收端、账号和高级设置。通知识别到明确验证码时增加“复制验证码”按钮。
- Android 接收端在息屏且使用电池时放慢空闲轮询，亮屏、充电、新消息或网络恢复时立即回到快速节奏。短信广播内持有唤醒锁，直到首轮转发完成。
- Windows 托盘图标按状态显示彩色圆点，窗口分“概览”和“设置”两页，最近短信改为表格。通知按发件人分开保留，并新增隐私预览三档，默认完整。
- Android 与 Windows 均升级为 v0.7.7（versionCode 17）。LAN v1 协议、端到端加密、先落盘再 ACK 和 Cloudflare 代码都没有改变。

## 验证

- Android：云端没有 Android SDK，只用 API 36 框架包做了类型检查，96 项 JVM 测试通过。没有运行 Gradle 构建、lint、签名检查和模拟器。
- Windows：在 Linux 上以 `GOOS=windows` 运行 vet、测试编译和 GUI 构建，均通过；27 项纯逻辑测试通过。没有在 Windows 上运行完整 `go test`。
- 真机待验项见 `HANDOFF.md` 的第二轮记录。

## 产物

| 文件 | 状态 | SHA-256 |
|---|---|---|
| MsgDock-Windows-v0.7.7.exe | 已在 Linux 交叉编译，未在 Windows 上运行 | D421925774E1795978697F580B082100B2143669EA8B0AA6ED4A264BD3656FCC |
| MsgDock-Android-v0.7.7-debug.apk | 待在本机构建 | - |

APK 必须用 v0.7.6 的同一个调试证书构建，证书 SHA-256 应为 `8b72e245377b56f06af95753d8fd396a880bf608a3161b6c175e443d94dbf82a`。签名不一致时，Android 不能覆盖升级，本机身份也无法继承。

## 发布步骤

1. 在 v0.7.6 的构建电脑上运行 `.\gradlew.bat --no-daemon :app:testDebugUnitTest :app:lintDebug :app:assembleDebug`，核对 APK 证书哈希。
2. 在 Windows 上运行 `go test ./...`，并按 `HANDOFF.md` 完成真机验收。
3. 创建 `release/msgdock-v0.7.7` 分支和 `v0.7.7` 标签，上传 APK、EXE、源码 ZIP 和 SHA-256 清单。
4. 匿名下载核对哈希，再按 `AGENTS.md` 更新网站下载链接，并在本文件补记发布核验。
