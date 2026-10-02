//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"golang.org/x/sys/windows"
)

const singleInstanceName = `Local\XgyLanSmsReceiver`

// Visual constants. Colors other than the shared state palette are neutral
// grays so the state dot and the needs-action markers are the only color.
const (
	uiMargin       = 16
	uiSpacing      = 12
	trayIconSize   = 16 // logical pixels; rendered at the tray's DPI
	windowIconSize = 32
	smsBodyLimit   = 160
	ipCacheWindow  = 5 * time.Second
)

var (
	colorMuted = walk.RGB(0x6B, 0x72, 0x80)
	colorFaint = walk.RGB(0x9C, 0xA3, 0xAF)
)

// actionRow is one prebuilt needs-action line; rows are shown or hidden
// instead of being created at runtime so layout stays stable.
type actionRow struct {
	row    *walk.Composite
	text   *walk.Label
	button *walk.PushButton
}

type desktopUI struct {
	stateMu    sync.RWMutex
	app        *App
	window     *walk.MainWindow
	notifyIcon *walk.NotifyIcon
	icons      statusIconCache
	tabs       *walk.TabWidget

	// 概览
	statusDot     *walk.Label
	statusTitle   *walk.Label
	statusReason  *walk.Label
	statusLast    *walk.Label
	actionPanel   *walk.Composite
	actionRows    map[syncActionKind]*actionRow
	smsTable      *walk.TableView
	smsModel      *smsTableModel
	smsEmpty      *walk.Label
	lanCodeLabel  *walk.Label
	lanAddress    *walk.Label
	cloudCode     *walk.Label
	cloudStatus   *walk.Label
	pairButton    *walk.PushButton
	copyCloudCode *walk.PushButton
	accountLine   *walk.Label

	// 设置
	accountStatus       *walk.Label
	accountIdentifier   *walk.LineEdit
	accountUsername     *walk.LineEdit
	accountEmail        *walk.LineEdit
	accountPassword     *walk.LineEdit
	accountDevices      *walk.TextEdit
	accountLogin        *walk.PushButton
	accountRegister     *walk.PushButton
	accountLogout       *walk.PushButton
	accountRefresh      *walk.PushButton
	accountRemove       *walk.PushButton
	accountLoginPanel   *walk.Composite
	accountSessionPanel *walk.Composite
	autoStartCheck      *walk.CheckBox
	trayFallbackCheck   *walk.CheckBox
	previewButtons      map[notificationPreview]*walk.RadioButton
	relayEdit           *walk.LineEdit

	// status bar: a quiet line that confirms copies and reminds that closing
	// the window keeps the receiver in the tray
	statusItem   *walk.StatusBarItem
	copyNotice   string
	copyNoticeAt time.Time

	// bookkeeping
	lastToolTip       string
	lastTrayLevel     syncLevel
	trayLevelSet      bool
	lastRecentKey     string
	lastIP            string
	lastIPAt          time.Time
	updatingSettings  bool
	pairingInFlight   bool
	accountBusy       bool
	exiting           bool
	lastTrayFallback  time.Time
	lastTrayMessageID string
	lastSettingProbe  time.Time
}

const (
	copyNoticeDuration = 4 * time.Second
	statusBarIdleText  = "关闭窗口后，MsgDock 继续在托盘接收短信。"
	// settingProbeInterval paces the read of Windows' notification setting;
	// each push also refreshes it, so this only covers quiet periods.
	settingProbeInterval = 30 * time.Second
)

// smsTableModel backs the recent-SMS table. Rows are newest first, matching
// App.recent; now is frozen per refresh so the time column is consistent.
type smsTableModel struct {
	walk.TableModelBase
	items []SMS
	now   time.Time
}

func (m *smsTableModel) RowCount() int {
	return len(m.items)
}

func (m *smsTableModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return ""
	}
	sms := m.items[row]
	switch col {
	case 0:
		return formatSMSTime(sms.ReceivedAt, m.now)
	case 1:
		return sms.From
	default:
		return singleLine(sms.Text, smsBodyLimit)
	}
}

func testSMS() SMS {
	return SMS{
		From:       "MsgDock 测试",
		Text:       "这是原生 Windows 通知测试，验证码 123456。",
		ReceivedAt: time.Now().UnixMilli(),
		Device:     "Windows",
	}
}

func newDesktopUI(app *App) (*desktopUI, error) {
	autoStartEnabled, err := app.autoStartEnabled()
	if err != nil {
		log.Printf("read Windows auto-start setting failed: %v", err)
	}
	ui := &desktopUI{
		app:              app,
		updatingSettings: true,
		smsModel:         &smsTableModel{now: time.Now()},
		actionRows:       make(map[syncActionKind]*actionRow),
		previewButtons:   make(map[notificationPreview]*walk.RadioButton),
	}
	rows := map[syncActionKind]*actionRow{
		syncActionRelogin:       {},
		syncActionNotifications: {},
		syncActionConnectRemote: {},
	}
	actionRowWidget := func(kind syncActionKind, onClick func()) Widget {
		row := rows[kind]
		ui.actionRows[kind] = row
		return Composite{
			AssignTo: &row.row,
			Visible:  false,
			Layout:   HBox{MarginsZero: true, Spacing: 10},
			Children: []Widget{
				Label{Text: "●", TextColor: walk.RGB(syncAttention.RGB()), Font: Font{PointSize: 9}},
				Label{AssignTo: &row.text, StretchFactor: 1},
				PushButton{AssignTo: &row.button, MinSize: Size{Width: 96}, OnClicked: onClick},
			},
		}
	}
	currentPreview := app.notificationPreview()
	var previewFullButton, previewSenderButton, previewMinimalButton *walk.RadioButton
	previewRadio := func(assign **walk.RadioButton, preview notificationPreview) Widget {
		return RadioButton{
			AssignTo: assign,
			Text:     preview.Label(),
			OnClicked: func() {
				if ui.updatingSettings {
					return
				}
				if err := app.setNotificationPreview(preview); err != nil {
					walk.MsgBox(ui.window, appName, "隐私预览设置保存失败：\r\n"+err.Error(), walk.MsgBoxIconError)
				}
				ui.refresh()
			},
		}
	}

	err = (MainWindow{
		AssignTo: &ui.window,
		// Declarative Walk treats a nil Visible property as visible. Keep the
		// native window hidden during construction; manual startup explicitly
		// calls showStatus, while --tray never flashes the window.
		Visible: false,
		Title:   appName,
		MinSize: Size{Width: 600, Height: 520},
		Size:    Size{Width: 720, Height: 680},
		Layout:  VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}},
		StatusBarItems: []StatusBarItem{
			{AssignTo: &ui.statusItem, Text: statusBarIdleText},
		},
		Children: []Widget{
			TabWidget{
				AssignTo: &ui.tabs,
				Pages: []TabPage{
					{
						Title:  "概览",
						Layout: VBox{Margins: Margins{Left: uiMargin, Top: uiMargin, Right: uiMargin, Bottom: uiMargin}, Spacing: uiSpacing},
						Children: []Widget{
							// 状态条
							Composite{
								Layout: HBox{MarginsZero: true, Spacing: 12},
								Children: []Widget{
									Label{
										AssignTo:  &ui.statusDot,
										Text:      "●",
										Font:      Font{PointSize: 20},
										TextColor: walk.RGB(syncNormal.RGB()),
									},
									Composite{
										Layout: VBox{MarginsZero: true, Spacing: 2},
										Children: []Widget{
											Label{AssignTo: &ui.statusTitle, Text: "正在检查同步状态…", Font: Font{PointSize: 15, Bold: true}},
											Label{AssignTo: &ui.statusReason, Text: ""},
											Label{AssignTo: &ui.statusLast, Text: "", TextColor: colorMuted},
										},
									},
									HSpacer{},
								},
							},
							// 待处理列表
							Composite{
								AssignTo: &ui.actionPanel,
								Visible:  false,
								Layout:   VBox{MarginsZero: true, Spacing: 6},
								Children: []Widget{
									actionRowWidget(syncActionRelogin, func() { ui.openSettings(true) }),
									actionRowWidget(syncActionNotifications, func() {
										if err := openWindowsNotificationSettings(); err != nil {
											walk.MsgBox(ui.window, appName, "打开 Windows 通知设置失败：\r\n"+err.Error(), walk.MsgBoxIconError)
										}
									}),
									actionRowWidget(syncActionConnectRemote, func() { ui.openSettings(true) }),
								},
							},
							// 最近短信
							GroupBox{
								Title:         "最近短信",
								StretchFactor: 1,
								Layout:        VBox{Margins: Margins{Left: 12, Top: 10, Right: 12, Bottom: 10}, Spacing: 8},
								Children: []Widget{
									Label{
										AssignTo:  &ui.smsEmpty,
										Text:      emptyInboxText(false),
										TextColor: colorMuted,
									},
									TableView{
										AssignTo:                    &ui.smsTable,
										Model:                       ui.smsModel,
										StretchFactor:               1,
										MinSize:                     Size{Height: 150},
										AlternatingRowBG:            true,
										LastColumnStretched:         true,
										NotSortableByHeaderClick:    true,
										SelectionHiddenWithoutFocus: false,
										Columns: []TableViewColumn{
											{Title: "时间", Width: 96},
											{Title: "来源", Width: 150},
											{Title: "正文"},
										},
										OnItemActivated: func() { ui.copySelectedSMS(false) },
										ContextMenuItems: []MenuItem{
											Action{Text: "复制验证码", OnTriggered: func() { ui.copySelectedSMS(false) }},
											Action{Text: "复制全文", OnTriggered: func() { ui.copySelectedSMS(true) }},
										},
									},
									Label{
										Text:      fmt.Sprintf("双击复制验证码，右键复制全文。显示最近 %d 条；每条先写入本地历史，再向手机确认。", historyDisplayLimit),
										TextColor: colorFaint,
									},
								},
							},
							// 连接手机
							GroupBox{
								Title:  "连接手机",
								Layout: VBox{Margins: Margins{Left: 12, Top: 10, Right: 12, Bottom: 10}, Spacing: 6},
								Children: []Widget{
									Composite{
										Layout: HBox{MarginsZero: true, Spacing: 12},
										Children: []Widget{
											Composite{
												Layout: VBox{MarginsZero: true, Spacing: 0},
												Children: []Widget{
													Label{Text: "局域网配对码", TextColor: colorMuted},
													Label{AssignTo: &ui.lanCodeLabel, Text: formatPairCode(app.pairCode()), Font: Font{PointSize: 26, Bold: true}},
												},
											},
											Composite{
												Layout: VBox{MarginsZero: true, Spacing: 0},
												Children: []Widget{
													VSpacer{},
													PushButton{Text: "复制", OnClicked: func() { ui.copyText("局域网配对码", app.pairCode()) }},
												},
											},
											HSpacer{},
										},
									},
									Label{AssignTo: &ui.lanAddress, TextColor: colorMuted},
									Label{Text: "手机和电脑连同一个 Wi‑Fi，在手机端“添加接收端”里扫描或输入这个码即可直连。首次运行若 Windows 防火墙询问，请允许专用网络。", TextColor: colorMuted},
									HSeparator{},
									Composite{
										Layout: HBox{MarginsZero: true, Spacing: 12},
										Children: []Widget{
											Composite{
												Layout: VBox{MarginsZero: true, Spacing: 0},
												Children: []Widget{
													Label{Text: "云配对码", TextColor: colorMuted},
													Label{AssignTo: &ui.cloudCode, Text: "未创建", Font: Font{PointSize: 18, Bold: true}},
												},
											},
											Composite{
												Layout: VBox{MarginsZero: true, Spacing: 0},
												Children: []Widget{
													VSpacer{},
													Composite{
														Layout: HBox{MarginsZero: true, Spacing: 8},
														Children: []Widget{
															PushButton{
																AssignTo:  &ui.pairButton,
																Text:      cloudPairButtonText(app.cloudCredentials()),
																Enabled:   !cloudPairingPending(app.cloudCredentials()),
																OnClicked: func() { ui.startCloudPairing() },
															},
															PushButton{
																AssignTo: &ui.copyCloudCode,
																Text:     "复制",
																OnClicked: func() {
																	credentials := app.cloudCredentials()
																	if app.cloudClient() == nil || !cloudPairingPending(credentials) {
																		walk.MsgBox(ui.window, appName, "请先开始云配对。", walk.MsgBoxIconInformation)
																		return
																	}
																	ui.copyText("云配对码", credentials.PairCode)
																},
															},
														},
													},
												},
											},
											HSpacer{},
										},
									},
									Label{AssignTo: &ui.cloudStatus, TextColor: colorMuted},
									HSeparator{},
									Label{AssignTo: &ui.accountLine, Text: "账号：未登录"},
								},
							},
						},
					},
					{
						Title:  "设置",
						Layout: VBox{MarginsZero: true},
						Children: []Widget{
							ScrollView{
								HorizontalFixed: true,
								Layout:          VBox{Margins: Margins{Left: uiMargin, Top: uiMargin, Right: uiMargin, Bottom: uiMargin}, Spacing: uiSpacing},
								Children: []Widget{
									GroupBox{
										Title:  "账号",
										Layout: VBox{Margins: Margins{Left: 12, Top: 10, Right: 12, Bottom: 10}, Spacing: 8},
										Children: []Widget{
											Label{AssignTo: &ui.accountStatus, Text: "账号：未登录"},
											Composite{
												AssignTo: &ui.accountLoginPanel,
												Layout:   VBox{MarginsZero: true, Spacing: 8},
												Children: []Widget{
													Label{Text: "登录后，手机不在同一 Wi‑Fi 时也能通过互联网收到短信。", TextColor: colorMuted},
													Composite{
														Layout: Grid{Columns: 2, MarginsZero: true, Spacing: 8},
														Children: []Widget{
															Label{Text: "用户名或邮箱"},
															LineEdit{AssignTo: &ui.accountIdentifier, CueBanner: "用户名或邮箱"},
															Label{Text: "密码"},
															LineEdit{AssignTo: &ui.accountPassword, CueBanner: "密码", PasswordMode: true},
														},
													},
													Composite{
														Layout: HBox{MarginsZero: true, Spacing: 8},
														Children: []Widget{
															PushButton{AssignTo: &ui.accountLogin, Text: "登录", MinSize: Size{Width: 96}, OnClicked: func() { ui.loginAccount() }},
															HSpacer{},
														},
													},
													Label{Text: "还没有账号？填写用户名和邮箱，用上面的密码注册。", TextColor: colorMuted},
													Composite{
														Layout: Grid{Columns: 2, MarginsZero: true, Spacing: 8},
														Children: []Widget{
															Label{Text: "用户名"},
															LineEdit{AssignTo: &ui.accountUsername, CueBanner: "用户名"},
															Label{Text: "邮箱"},
															LineEdit{AssignTo: &ui.accountEmail, CueBanner: "邮箱"},
														},
													},
													Composite{
														Layout: HBox{MarginsZero: true, Spacing: 8},
														Children: []Widget{
															PushButton{AssignTo: &ui.accountRegister, Text: "注册并登录", MinSize: Size{Width: 96}, OnClicked: func() { ui.registerAccount() }},
															HSpacer{},
														},
													},
												},
											},
											Composite{
												AssignTo: &ui.accountSessionPanel,
												Layout:   VBox{MarginsZero: true, Spacing: 8},
												Children: []Widget{
													Composite{
														Layout: HBox{MarginsZero: true, Spacing: 8},
														Children: []Widget{
															PushButton{AssignTo: &ui.accountLogout, Text: "退出账号", OnClicked: func() { ui.logoutAccount() }},
															PushButton{AssignTo: &ui.accountRefresh, Text: "刷新设备", OnClicked: func() { ui.refreshAccountDevices() }},
															PushButton{AssignTo: &ui.accountRemove, Text: "移除本机设备", OnClicked: func() { ui.removeAccountDevice() }},
															HSpacer{},
														},
													},
													Label{Text: "已登录的设备（* 为本机）", TextColor: colorMuted},
													TextEdit{AssignTo: &ui.accountDevices, ReadOnly: true, VScroll: true, MinSize: Size{Height: 74}},
												},
											},
										},
									},
									GroupBox{
										Title:  "开机自启",
										Layout: VBox{Margins: Margins{Left: 12, Top: 10, Right: 12, Bottom: 10}, Spacing: 6},
										Children: []Widget{
											CheckBox{
												AssignTo: &ui.autoStartCheck,
												Text:     "登录 Windows 时自动在托盘启动",
												Checked:  autoStartEnabled,
												OnCheckedChanged: func() {
													if ui.updatingSettings {
														return
													}
													if err := app.setAutoStartEnabled(ui.autoStartCheck.Checked()); err != nil {
														walk.MsgBox(ui.window, appName, "开机自动启动设置失败：\r\n"+err.Error(), walk.MsgBoxIconError)
													}
													ui.refresh()
												},
											},
											Label{Text: "不弹出窗口，只在托盘等待短信。", TextColor: colorMuted},
										},
									},
									GroupBox{
										Title:  "通知",
										Layout: VBox{Margins: Margins{Left: 12, Top: 10, Right: 12, Bottom: 10}, Spacing: 8},
										Children: []Widget{
											CheckBox{
												AssignTo: &ui.trayFallbackCheck,
												Text:     "Windows 通知失败时改用托盘气泡提醒",
												Checked:  app.trayFallbackEnabled(),
												OnCheckedChanged: func() {
													if ui.updatingSettings {
														return
													}
													if err := app.setTrayFallbackEnabled(ui.trayFallbackCheck.Checked()); err != nil {
														walk.MsgBox(ui.window, appName, "托盘提醒设置失败：\r\n"+err.Error(), walk.MsgBoxIconError)
													}
													ui.refresh()
												},
											},
											Composite{
												Layout: HBox{MarginsZero: true, Spacing: 8},
												Children: []Widget{
													PushButton{
														Text: "打开 Windows 通知设置",
														OnClicked: func() {
															if err := openWindowsNotificationSettings(); err != nil {
																walk.MsgBox(ui.window, appName, "打开 Windows 通知设置失败：\r\n"+err.Error(), walk.MsgBoxIconError)
															}
														},
													},
													PushButton{
														Text: "发送测试通知",
														OnClicked: func() {
															if err := app.showSMSNotification(testSMS()); err != nil {
																walk.MsgBox(ui.window, appName, "测试通知发送失败：\r\n"+err.Error(), walk.MsgBoxIconError)
															}
														},
													},
													HSpacer{},
												},
											},
											Label{Text: "横幅、声音和勿扰由 Windows 通知设置控制。", TextColor: colorMuted},
										},
									},
									GroupBox{
										Title:  "隐私预览",
										Layout: VBox{Margins: Margins{Left: 12, Top: 10, Right: 12, Bottom: 10}, Spacing: 8},
										Children: []Widget{
											Label{Text: "控制 Windows 通知里显示多少短信内容。验证码自动复制和“复制验证码”按钮在三档下都可用。", TextColor: colorMuted},
											Composite{
												Layout: HBox{MarginsZero: true, Spacing: 16},
												Children: []Widget{
													previewRadio(&previewFullButton, previewFull),
													previewRadio(&previewSenderButton, previewSender),
													previewRadio(&previewMinimalButton, previewMinimal),
													HSpacer{},
												},
											},
										},
									},
									GroupBox{
										Title:  "云端地址",
										Layout: VBox{Margins: Margins{Left: 12, Top: 10, Right: 12, Bottom: 10}, Spacing: 8},
										Children: []Widget{
											Label{Text: "配对云端 Relay URL（HTTPS，只保存密文和路由信息）", TextColor: colorMuted},
											Composite{
												Layout: HBox{MarginsZero: true, Spacing: 8},
												Children: []Widget{
													LineEdit{AssignTo: &ui.relayEdit, Text: app.relayURL(), StretchFactor: 1},
													PushButton{
														Text: "保存",
														OnClicked: func() {
															if err := app.setRelayURL(ui.relayEdit.Text()); err != nil {
																walk.MsgBox(ui.window, appName, "Relay URL 保存失败：\r\n"+err.Error(), walk.MsgBoxIconError)
																return
															}
															ui.refresh()
														},
													},
												},
											},
											Label{Text: "账号 API：" + defaultAccountAPIURL, TextColor: colorMuted},
										},
									},
									Label{
										Text:      fmt.Sprintf("%s v%s · 历史文件：%s", appName, appVersion, app.historyPath()),
										TextColor: colorFaint,
									},
								},
							},
						},
					},
				},
			},
		},
	}).Create()
	if err != nil {
		return nil, err
	}
	ui.previewButtons[previewFull] = previewFullButton
	ui.previewButtons[previewSender] = previewSenderButton
	ui.previewButtons[previewMinimal] = previewMinimalButton
	if button := ui.previewButtons[currentPreview]; button != nil {
		button.SetChecked(true)
	}
	ui.updatingSettings = false

	ui.window.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if ui.exiting {
			return
		}
		*canceled = true
		ui.window.Hide()
		ui.showTrayHintOnce()
	})

	ui.notifyIcon, err = walk.NewNotifyIcon(ui.window)
	if err != nil {
		ui.window.Dispose()
		return nil, err
	}
	status := ui.status()
	ui.applyTrayState(status)
	ui.notifyIcon.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			ui.showStatus()
		}
	})

	openAction := walk.NewAction()
	_ = openAction.SetText("打开 MsgDock")
	openAction.Triggered().Attach(ui.showStatus)
	if err := ui.notifyIcon.ContextMenu().Actions().Add(openAction); err != nil {
		ui.dispose()
		return nil, err
	}

	testAction := walk.NewAction()
	_ = testAction.SetText("发送测试通知")
	testAction.Triggered().Attach(func() {
		_ = app.showSMSNotification(testSMS())
	})
	if err := ui.notifyIcon.ContextMenu().Actions().Add(testAction); err != nil {
		ui.dispose()
		return nil, err
	}

	exitAction := walk.NewAction()
	_ = exitAction.SetText("退出")
	exitAction.Triggered().Attach(func() {
		ui.app.beginClosing()
		ui.exiting = true
		if ui.notifyIcon != nil {
			_ = ui.notifyIcon.SetVisible(false)
		}
		walk.App().Exit(0)
	})
	if err := ui.notifyIcon.ContextMenu().Actions().Add(exitAction); err != nil {
		ui.dispose()
		return nil, err
	}
	if err := ui.notifyIcon.SetVisible(true); err != nil {
		ui.dispose()
		return nil, err
	}

	ui.refresh()
	return ui, nil
}

// run pumps the message loop until 退出. showEvent (0 when unavailable) is the
// auto-reset event a second launch sets to bring this window forward; the
// caller owns its handle.
func (ui *desktopUI) run(showEvent windows.Handle) {
	done := make(chan struct{})
	var refreshWG sync.WaitGroup
	refreshWG.Add(1)
	go func() {
		defer refreshWG.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ui.stateMu.RLock()
				window := ui.window
				ui.stateMu.RUnlock()
				if window != nil && !ui.app.isClosing() {
					window.Synchronize(ui.refresh)
				}
			case <-done:
				return
			}
		}
	}()
	if showEvent != 0 {
		refreshWG.Add(1)
		go func() {
			defer refreshWG.Done()
			for {
				// The timeout is only a safety net; shutdown sets the event itself
				// so this loop ends at once.
				result, err := windows.WaitForSingleObject(showEvent, 500)
				select {
				case <-done:
					return
				default:
				}
				if err != nil {
					log.Printf("wait for show-window event failed: %v", err)
					return
				}
				if result != windows.WAIT_OBJECT_0 {
					continue
				}
				ui.stateMu.RLock()
				window := ui.window
				ui.stateMu.RUnlock()
				if window != nil && !ui.app.isClosing() {
					window.Synchronize(ui.showStatus)
				}
			}
		}()
	}
	ui.window.Run()
	close(done)
	if showEvent != 0 {
		_ = windows.SetEvent(showEvent)
	}
	refreshWG.Wait()
}

func (ui *desktopUI) dispose() {
	ui.stateMu.Lock()
	notifyIcon := ui.notifyIcon
	window := ui.window
	ui.notifyIcon = nil
	ui.window = nil
	ui.stateMu.Unlock()
	if notifyIcon != nil {
		_ = notifyIcon.SetVisible(false)
		notifyIcon.Dispose()
	}
	if window != nil {
		window.Dispose()
	}
	ui.icons.dispose()
}

func (ui *desktopUI) enqueueTrayFallback(id, title, body string) {
	if ui == nil || ui.app.isClosing() {
		return
	}
	ui.stateMu.RLock()
	window := ui.window
	icon := ui.notifyIcon
	if window == nil || icon == nil {
		ui.stateMu.RUnlock()
		return
	}
	window.Synchronize(func() {
		if ui.app.isClosing() {
			return
		}
		ui.stateMu.RLock()
		currentIcon := ui.notifyIcon
		ui.stateMu.RUnlock()
		if currentIcon == nil {
			return
		}
		if (id != "" && id == ui.lastTrayMessageID) || time.Since(ui.lastTrayFallback) < notificationPopupInterval {
			return
		}
		ui.lastTrayFallback = time.Now()
		ui.lastTrayMessageID = id
		if err := currentIcon.ShowInfo(title, body); err != nil {
			log.Printf("compatibility tray notification failed: %v", err)
		}
	})
	ui.stateMu.RUnlock()
}

func (ui *desktopUI) showStatus() {
	if ui.window == nil {
		return
	}
	hwnd := uintptr(ui.window.Handle())
	// A minimized window would stay in the taskbar after Show(); restore it so
	// a tray click or a second launch always ends with the window in front.
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		_, _, _ = procShowWindow.Call(hwnd, swRestore)
	}
	// Show first: refresh() only redraws a visible window.
	ui.window.Show()
	ui.refresh()
	_, _, _ = procSetForegroundWindow.Call(hwnd)
	_ = ui.window.Activate()
}

// showTrayHintOnce explains, the first time the window is closed on this
// installation, that MsgDock keeps running in the tray and where its icon is.
// Later closes stay silent; the flag is persisted in config.json.
func (ui *desktopUI) showTrayHintOnce() {
	if ui.notifyIcon == nil || ui.app.isClosing() || ui.app.trayHintShown() {
		return
	}
	if err := ui.app.markTrayHintShown(); err != nil {
		log.Printf("persist tray hint flag failed: %v", err)
		return
	}
	if err := ui.notifyIcon.ShowInfo("MsgDock 仍在运行",
		"窗口已收到托盘，短信照常通知。图标在任务栏右下角，可能收在“^”里；点击图标可重新打开窗口。"); err != nil {
		log.Printf("tray hint balloon failed: %v", err)
	}
}

// openSettings switches to the 设置 tab; focusLogin puts the caret into the
// login form when the user arrived from a needs-action button.
func (ui *desktopUI) openSettings(focusLogin bool) {
	if ui.window == nil || ui.tabs == nil {
		return
	}
	ui.showStatus()
	_ = ui.tabs.SetCurrentIndex(1)
	if focusLogin && ui.accountLoginPanel != nil && ui.accountLoginPanel.Visible() && ui.accountIdentifier != nil {
		if strings.TrimSpace(ui.accountIdentifier.Text()) == "" {
			_ = ui.accountIdentifier.SetFocus()
		} else if ui.accountPassword != nil {
			_ = ui.accountPassword.SetFocus()
		}
	}
}

// localAddress caches the interface scan so the 1 s tick does not enumerate
// adapters every second while the window is hidden.
func (ui *desktopUI) localAddress(now time.Time) string {
	if ui.lastIP == "" || now.Sub(ui.lastIPAt) > ipCacheWindow {
		ui.lastIP = localIPv4()
		ui.lastIPAt = now
	}
	return ui.lastIP
}

// status gathers one read-only snapshot for the five-state model.
func (ui *desktopUI) status() syncStatus {
	now := time.Now()
	in := syncInput{
		Now:            now,
		Offline:        ui.localAddress(now) == "0.0.0.0",
		Sending:        ui.app.isSending(),
		NotifyFailed:   ui.app.notifyFailed.Load(),
		NotifyDisabled: ui.app.notifyDisabled.Load(),
	}
	credentials := ui.app.accountCredentials()
	in.AccountLoggedIn = credentials.SessionToken != ""
	in.AccountName = accountStatusDetail(credentials)
	if account := ui.app.accountClient(); account != nil {
		snapshot := account.snapshot()
		in.AuthFailed = snapshot.State == "需要重新登录"
		in.Account = syncPath{
			Active:       credentials.DeviceToken != "" && !in.AuthFailed,
			Failing:      snapshot.State == "云同步重试中",
			Detail:       snapshot.Detail,
			Interval:     snapshot.Backoff,
			LastSuccess:  snapshot.LastSuccessAt,
			FailingSince: snapshot.FailingSince,
		}
	}
	if cloud := ui.app.cloudClient(); cloud != nil {
		snapshot := cloud.snapshot()
		in.CloudPaired = snapshot.Paired
		in.Cloud = syncPath{
			Active:       snapshot.Paired,
			Failing:      snapshot.State == "连接失败",
			Detail:       snapshot.Detail,
			Interval:     snapshot.Backoff,
			LastSuccess:  snapshot.LastPoll,
			FailingSince: snapshot.FailingSince,
		}
	}
	if pending, err := ui.app.pendingNotificationsSnapshot(); err == nil {
		in.Backlog = pendingBacklogCount(pending)
	}
	if latest, ok := ui.app.latestSMS(); ok {
		in.LastSMSAt = latest.ReceivedAt
		in.LastSMSDevice = latest.Device
	}
	return evaluateSyncState(in)
}

// applyTrayState swaps the tray and window icons only when the level changes
// and updates the tooltip only when its text changes.
func (ui *desktopUI) applyTrayState(status syncStatus) {
	if ui.notifyIcon == nil {
		return
	}
	if !ui.trayLevelSet || status.Level != ui.lastTrayLevel {
		trayIcon := ui.icons.icon(status.Level, trayIconSize, ui.notifyIcon.DPI())
		windowIcon := ui.icons.icon(status.Level, windowIconSize, 96)
		if trayIcon == nil && ui.notifyIcon.Icon() == nil {
			// Rendering failed before any icon was shown: a tray entry without an
			// icon is invisible, so fall back to the stock application icon.
			trayIcon = walk.IconApplication()
			if windowIcon == nil {
				windowIcon = trayIcon
			}
		}
		if trayIcon != nil {
			if err := ui.notifyIcon.SetIcon(trayIcon); err != nil {
				log.Printf("set tray icon failed: %v", err)
			}
		}
		if ui.window != nil && windowIcon != nil {
			if err := ui.window.SetIcon(windowIcon); err != nil {
				log.Printf("set window icon failed: %v", err)
			}
		}
		ui.lastTrayLevel = status.Level
		ui.trayLevelSet = true
	}
	if status.Tooltip != ui.lastToolTip {
		if err := ui.notifyIcon.SetToolTip(status.Tooltip); err != nil {
			log.Printf("set tray tooltip failed: %v", err)
		} else {
			ui.lastToolTip = status.Tooltip
		}
	}
}

func (ui *desktopUI) refresh() {
	if ui.window == nil || ui.app.isClosing() {
		return
	}
	status := ui.status()
	ui.applyTrayState(status)
	if now := time.Now(); now.Sub(ui.lastSettingProbe) >= settingProbeInterval {
		ui.lastSettingProbe = now
		go ui.app.probeNotificationSetting()
	}
	// The 1 s ticker keeps running while the window sits in the tray. Hidden
	// windows only need the tray; skipping the registry read and the table
	// update saves wakeups and keeps the reader's selection and scroll.
	if !ui.window.Visible() {
		return
	}
	ui.refreshStatusBar(status)
	ui.refreshSettings()
	ui.refreshConnections()
	ui.refreshRecent()
	ui.refreshStatusItem()
}

// refreshStatusItem shows the last copy confirmation for a few seconds, then
// returns to the standing tray reminder.
func (ui *desktopUI) refreshStatusItem() {
	if ui.statusItem == nil {
		return
	}
	text := statusBarIdleText
	if ui.copyNotice != "" && time.Since(ui.copyNoticeAt) < copyNoticeDuration {
		text = ui.copyNotice
	}
	_ = ui.statusItem.SetText(text)
}

func (ui *desktopUI) refreshStatusBar(status syncStatus) {
	if ui.statusDot != nil {
		color := walk.RGB(status.Level.RGB())
		if ui.statusDot.TextColor() != color {
			ui.statusDot.SetTextColor(color)
		}
	}
	if ui.statusTitle != nil {
		ui.statusTitle.SetText(status.Title)
	}
	if ui.statusReason != nil {
		ui.statusReason.SetText(status.Reason)
	}
	if ui.statusLast != nil {
		text := status.LastSMS
		if text == "" {
			text = "尚未收到短信"
		}
		ui.statusLast.SetText(text)
	}
	shown := make(map[syncActionKind]bool, len(status.Actions))
	for _, action := range status.Actions {
		row := ui.actionRows[action.Kind]
		if row == nil || row.row == nil {
			continue
		}
		shown[action.Kind] = true
		row.text.SetText(action.Text)
		row.button.SetText(action.Button)
		if !row.row.Visible() {
			row.row.SetVisible(true)
		}
	}
	for kind, row := range ui.actionRows {
		if row.row != nil && !shown[kind] && row.row.Visible() {
			row.row.SetVisible(false)
		}
	}
	if ui.actionPanel != nil && ui.actionPanel.Visible() != (len(status.Actions) > 0) {
		ui.actionPanel.SetVisible(len(status.Actions) > 0)
	}
}

func (ui *desktopUI) refreshSettings() {
	ui.updatingSettings = true
	defer func() { ui.updatingSettings = false }()
	// The Run key is only re-read while the 设置 tab is showing, so the 1 s tick
	// on the 概览 tab never touches the registry.
	if ui.autoStartCheck != nil && ui.tabs != nil && ui.tabs.CurrentIndex() == 1 {
		if enabled, err := ui.app.autoStartEnabled(); err != nil {
			log.Printf("read Windows auto-start setting failed: %v", err)
		} else if ui.autoStartCheck.Checked() != enabled {
			ui.autoStartCheck.SetChecked(enabled)
		}
	}
	if ui.trayFallbackCheck != nil {
		enabled := ui.app.trayFallbackEnabled()
		if ui.trayFallbackCheck.Checked() != enabled {
			ui.trayFallbackCheck.SetChecked(enabled)
		}
	}
	current := ui.app.notificationPreview()
	for preview, button := range ui.previewButtons {
		if button != nil && button.Checked() != (preview == current) {
			button.SetChecked(preview == current)
		}
	}
	if account := ui.app.accountClient(); account != nil {
		credentials := ui.app.accountCredentials()
		snapshot := account.snapshot()
		if ui.accountStatus != nil {
			ui.accountStatus.SetText("账号：" + accountStatusText(snapshot, credentials))
		}
		if ui.accountDevices != nil {
			if text := formatAccountDevices(account.devicesSnapshot(), credentials.DeviceID); text != ui.accountDevices.Text() {
				_ = ui.accountDevices.SetText(text)
			}
		}
		busy := ui.accountBusy
		// An expired session keeps its token until the user logs in again, so the
		// form must come back while 需要重新登录 is showing.
		loggedIn := credentials.SessionToken != "" && snapshot.State != "需要重新登录"
		if ui.accountLoginPanel != nil && ui.accountLoginPanel.Visible() == loggedIn {
			ui.accountLoginPanel.SetVisible(!loggedIn)
		}
		if !loggedIn && ui.accountIdentifier != nil && ui.accountIdentifier.Text() == "" {
			if known := accountStatusDetail(credentials); known != "" {
				_ = ui.accountIdentifier.SetText(known)
			}
		}
		if ui.accountSessionPanel != nil && ui.accountSessionPanel.Visible() != loggedIn {
			ui.accountSessionPanel.SetVisible(loggedIn)
		}
		if ui.accountLogin != nil {
			ui.accountLogin.SetEnabled(!busy)
		}
		if ui.accountRegister != nil {
			ui.accountRegister.SetEnabled(!busy)
		}
		if ui.accountLogout != nil {
			ui.accountLogout.SetEnabled(!busy && loggedIn)
		}
		if ui.accountRefresh != nil {
			ui.accountRefresh.SetEnabled(!busy && loggedIn)
		}
		if ui.accountRemove != nil {
			ui.accountRemove.SetEnabled(!busy && loggedIn && credentials.DeviceID != "")
		}
	}
}

func (ui *desktopUI) refreshConnections() {
	now := time.Now()
	if ui.lanCodeLabel != nil {
		ui.lanCodeLabel.SetText(formatPairCode(ui.app.pairCode()))
	}
	if ui.lanAddress != nil {
		host, _ := os.Hostname()
		ui.lanAddress.SetText(fmt.Sprintf("本机 %s · http://%s:%d", host, ui.localAddress(now), httpPort))
	}
	credentials := ui.app.cloudCredentials()
	pendingPair := cloudPairingPending(credentials)
	if ui.pairButton != nil {
		ui.pairButton.SetText(cloudPairButtonText(credentials))
		ui.pairButton.SetEnabled(!ui.pairingInFlight && !pendingPair)
	}
	if ui.copyCloudCode != nil && ui.copyCloudCode.Visible() != pendingPair {
		ui.copyCloudCode.SetVisible(pendingPair)
	}
	if ui.cloudCode != nil {
		ui.cloudCode.SetText(cloudPairCodeText(credentials))
	}
	if ui.cloudStatus != nil {
		if cloud := ui.app.cloudClient(); cloud == nil {
			ui.cloudStatus.SetText("云端：尚未启动 · " + cloudPairStatusText(credentials, cloudStatusSnapshot{}))
		} else {
			snapshot := cloud.snapshot()
			detail := cloudPairStatusText(credentials, snapshot)
			if snapshot.Paired && snapshot.Backoff > 0 {
				detail += fmt.Sprintf(" · 每 %s 检查", snapshot.Backoff.Round(time.Second))
			}
			ui.cloudStatus.SetText("云端：" + detail + " · 只保存密文，电脑和手机端到端加密。")
		}
	}
	if ui.accountLine != nil {
		account := ui.app.accountCredentials()
		switch {
		case account.SessionToken == "":
			ui.accountLine.SetText("账号：未登录 · 在“设置”里登录，离开局域网也能收到。")
		default:
			if client := ui.app.accountClient(); client != nil {
				ui.accountLine.SetText("账号：" + accountStatusText(client.snapshot(), account))
			} else {
				ui.accountLine.SetText("账号：已登录 " + accountStatusDetail(account))
			}
		}
	}
}

// refreshRecent re-publishes the table only when history changed and keeps the
// reader's selection on the same message.
func (ui *desktopUI) refreshRecent() {
	if ui.smsTable == nil || ui.smsModel == nil {
		return
	}
	now := time.Now()
	recent := ui.app.recentSnapshot()
	// The day is part of the key so the time column switches from "15:04" to a
	// dated form after midnight even when no message arrived.
	recentKey := fmt.Sprintf("%d|%d", len(recent), now.YearDay())
	if len(recent) > 0 {
		recentKey += "|" + recent[0].ID + "|" + fmt.Sprint(recent[0].ReceivedAt) + "|" + recent[len(recent)-1].ID
	}
	if len(recent) == 0 && ui.smsEmpty != nil {
		// Only the empty state depends on whether a remote path exists, so the
		// text is refreshed outside the key.
		remote := ui.app.accountCredentials().SessionToken != "" || cloudCredentialsReady(ui.app.cloudCredentials())
		ui.smsEmpty.SetText(emptyInboxText(remote))
	}
	if recentKey == ui.lastRecentKey {
		return
	}
	ui.lastRecentKey = recentKey
	selectedID := ""
	if index := ui.smsTable.CurrentIndex(); index >= 0 && index < len(ui.smsModel.items) {
		selectedID = ui.smsModel.items[index].ID
	}
	ui.smsModel.items = recent
	ui.smsModel.now = now
	ui.smsModel.PublishRowsReset()
	if selectedID != "" {
		for index, sms := range recent {
			if sms.ID == selectedID {
				_ = ui.smsTable.SetCurrentIndex(index)
				ui.smsTable.EnsureItemVisible(index)
				break
			}
		}
	}
	if ui.smsEmpty != nil && ui.smsEmpty.Visible() != (len(recent) == 0) {
		ui.smsEmpty.SetVisible(len(recent) == 0)
	}
}

// emptyInboxText is the table's empty state. Without any remote path the
// only way to get a first message is the LAN pair code shown below, so it
// says so; otherwise it just explains what will appear here.
func emptyInboxText(remoteConfigured bool) string {
	if remoteConfigured {
		return "还没有短信。手机转发的第一条短信会出现在这里，并弹出 Windows 通知。"
	}
	return "还没有短信。先在手机端“添加接收端”里输入下方的局域网配对码；第一条短信会出现在这里，并弹出 Windows 通知。"
}

// copySelectedSMS copies the OTP of the selected row, or the whole text when
// fullText is set or no code is present. Manual copies keep the legacy
// extractor so a user can still grab an unlabeled digit run.
func (ui *desktopUI) copySelectedSMS(fullText bool) {
	if ui.smsTable == nil || ui.smsModel == nil {
		return
	}
	index := ui.smsTable.CurrentIndex()
	if index < 0 || index >= len(ui.smsModel.items) {
		return
	}
	sms := ui.smsModel.items[index]
	if !fullText {
		if code := extractVerificationCode(sms.Text); code != "" {
			ui.copyText("验证码", code)
			return
		}
	}
	ui.copyText("短信全文", sms.Text)
}

func formatAccountDevices(devices []accountDevice, currentID string) string {
	if len(devices) == 0 {
		return "尚未读取设备列表。登录后点击“刷新设备”。"
	}
	var out strings.Builder
	for index, device := range devices {
		if index > 0 {
			out.WriteString("\r\n")
		}
		marker := "  "
		if device.ID == currentID {
			marker = "* "
		}
		name := device.Name
		if name == "" {
			name = device.ID
		}
		out.WriteString(marker)
		out.WriteString(name)
		out.WriteString(" · ")
		if device.Type == "" {
			out.WriteString("unknown")
		} else {
			out.WriteString(device.Type)
		}
		out.WriteString(" · ")
		if device.LastSeenAt > 0 {
			out.WriteString(time.UnixMilli(device.LastSeenAt).Format("2006-01-02 15:04:05"))
		} else {
			out.WriteString("最后在线未知")
		}
	}
	return out.String()
}

// runAccountAction runs one user-initiated account call off the UI thread.
// Failures of the click may pop a dialog; success shows up in the status line.
func (ui *desktopUI) runAccountAction(action func(*accountClient) error) {
	if ui == nil || ui.app == nil || ui.app.isClosing() || ui.accountBusy {
		return
	}
	client := ui.app.accountClient()
	if client == nil {
		walk.MsgBox(ui.window, appName, "账号同步尚未启动。", walk.MsgBoxIconError)
		return
	}
	ui.accountBusy = true
	ui.refresh()
	go func() {
		err := action(client)
		if errors.Is(err, context.Canceled) && ui.app.isClosing() {
			return
		}
		ui.stateMu.RLock()
		window := ui.window
		ui.stateMu.RUnlock()
		if window == nil || ui.app.isClosing() {
			return
		}
		window.Synchronize(func() {
			if ui.app.isClosing() || ui.window == nil {
				return
			}
			ui.accountBusy = false
			ui.refresh()
			if err != nil {
				walk.MsgBox(ui.window, appName, "账号操作失败：\r\n"+accountErrorDetail(err), walk.MsgBoxIconError)
			}
		})
	}()
}

func (ui *desktopUI) loginAccount() {
	if ui.accountIdentifier == nil || ui.accountPassword == nil {
		return
	}
	identifier := strings.TrimSpace(ui.accountIdentifier.Text())
	password := ui.accountPassword.Text()
	if identifier == "" || password == "" {
		walk.MsgBox(ui.window, appName, "请输入用户名或邮箱和密码。", walk.MsgBoxIconInformation)
		return
	}
	ui.runAccountAction(func(client *accountClient) error {
		err := client.login(ui.app.shutdownContext(), identifier, password)
		if err == nil {
			ui.clearPassword()
		}
		return err
	})
}

func (ui *desktopUI) registerAccount() {
	if ui.accountUsername == nil || ui.accountEmail == nil || ui.accountPassword == nil {
		return
	}
	username := strings.TrimSpace(ui.accountUsername.Text())
	email := strings.TrimSpace(ui.accountEmail.Text())
	password := ui.accountPassword.Text()
	if username == "" || email == "" || password == "" {
		walk.MsgBox(ui.window, appName, "注册时请填写用户名、邮箱和密码。", walk.MsgBoxIconInformation)
		return
	}
	ui.runAccountAction(func(client *accountClient) error {
		err := client.register(ui.app.shutdownContext(), username, email, password)
		if err == nil {
			ui.clearPassword()
		}
		return err
	})
}

// clearPassword runs on the worker goroutine, so it hops to the UI thread.
func (ui *desktopUI) clearPassword() {
	ui.stateMu.RLock()
	window := ui.window
	ui.stateMu.RUnlock()
	if window == nil {
		return
	}
	window.Synchronize(func() {
		if ui.accountPassword != nil {
			_ = ui.accountPassword.SetText("")
		}
	})
}

func (ui *desktopUI) logoutAccount() {
	result := walk.MsgBox(ui.window, appName, "退出账号后将停止互联网短信同步，局域网和云配对不受影响。确定退出吗？", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion)
	if result != walk.DlgCmdYes {
		return
	}
	ui.runAccountAction(func(client *accountClient) error {
		return client.logout(ui.app.shutdownContext())
	})
}

func (ui *desktopUI) refreshAccountDevices() {
	ui.runAccountAction(func(client *accountClient) error {
		return client.refreshDevices(ui.app.shutdownContext())
	})
}

func (ui *desktopUI) removeAccountDevice() {
	credentials := ui.app.accountCredentials()
	if credentials.DeviceID == "" {
		walk.MsgBox(ui.window, appName, "当前没有已注册的 Windows 设备。", walk.MsgBoxIconInformation)
		return
	}
	result := walk.MsgBox(ui.window, appName, "移除本机设备后，互联网短信同步会停止，确认继续吗？", walk.MsgBoxYesNo|walk.MsgBoxIconWarning)
	if result != walk.DlgCmdYes {
		return
	}
	ui.runAccountAction(func(client *accountClient) error {
		return client.removeDevice(ui.app.shutdownContext(), credentials.DeviceID)
	})
}

func (ui *desktopUI) startCloudPairing() {
	cloud := ui.app.cloudClient()
	if ui.app.isClosing() || cloud == nil || ui.pairButton == nil {
		return
	}
	credentials := ui.app.cloudCredentials()
	if cloudPairingPending(credentials) || ui.pairingInFlight {
		ui.refresh()
		return
	}
	force := cloudCredentialsReady(credentials)
	if force {
		result := walk.MsgBox(ui.window, appName,
			"当前设备已经完成云配对。重新云配对会替换旧凭据；旧设备以及尚未接收的云消息可能无法继续解密。\r\n\r\n确定要继续并创建新的云配对码吗？",
			walk.MsgBoxYesNo|walk.MsgBoxIconWarning|walk.MsgBoxDefButton2)
		if result != walk.DlgCmdYes {
			return
		}
	}
	ui.pairingInFlight = true
	ui.pairButton.SetEnabled(false)
	go func() {
		var err error
		if force {
			err = cloud.startPairingForced()
		} else {
			err = cloud.startPairing()
		}
		ui.stateMu.RLock()
		window := ui.window
		ui.stateMu.RUnlock()
		if window == nil || ui.app.isClosing() {
			return
		}
		window.Synchronize(func() {
			if ui.app.isClosing() || ui.window == nil {
				return
			}
			ui.pairingInFlight = false
			if err != nil {
				walk.MsgBox(ui.window, appName, "云配对启动失败：\r\n"+err.Error(), walk.MsgBoxIconError)
			}
			ui.refresh()
		})
	}()
}

func cloudPairButtonText(credentials CloudCredentials) string {
	if cloudPairingPending(credentials) {
		return "配对进行中"
	}
	if cloudCredentialsReady(credentials) {
		return "重新云配对"
	}
	return "开始云配对"
}

func cloudPairCodeText(credentials CloudCredentials) string {
	switch {
	case cloudPairingPending(credentials):
		return formatPairCode(credentials.PairCode)
	case cloudCredentialsReady(credentials):
		if credentials.LastPairCode == "" {
			return "未保留（旧版本）"
		}
		return formatPairCode(credentials.LastPairCode) + "（已失效）"
	default:
		return "未创建"
	}
}

func cloudPairStatusText(credentials CloudCredentials, snapshot cloudStatusSnapshot) string {
	if cloudPairingPending(credentials) {
		if snapshot.State == "清理失败" && snapshot.Detail != "" {
			return snapshot.State + " · " + snapshot.Detail + " · 当前码 " + formatPairCode(credentials.PairCode)
		}
		detail := "等待手机确认 · 在手机端输入上面的云配对码"
		if credentials.PairExpiresAt > 0 {
			detail += " · " + countdownText(time.Until(time.UnixMilli(credentials.PairExpiresAt)))
		}
		return detail
	}
	if cloudCredentialsReady(credentials) {
		if credentials.LastPairCode == "" {
			return "已配对 · 上次云配对码未保留（旧版本配置）"
		}
		return "已配对 · 上次云配对码 " + formatPairCode(credentials.LastPairCode) + " 已失效"
	}
	if snapshot.State != "" && snapshot.State != "未配对" {
		if snapshot.Detail != "" {
			return snapshot.State + " · " + snapshot.Detail
		}
		return snapshot.State
	}
	return "未配对 · 点击“开始云配对”生成一次性云配对码"
}

func (ui *desktopUI) onSMS(sms SMS) {
	if ui.app.isClosing() {
		return
	}
	ui.stateMu.RLock()
	window := ui.window
	ui.stateMu.RUnlock()
	if window == nil {
		return
	}
	window.Synchronize(func() {
		if ui.app.isClosing() || ui.window == nil {
			return
		}
		ui.refresh()
	})
}

func (ui *desktopUI) handleToastAction(arguments string) {
	if ui.app.isClosing() {
		return
	}
	ui.stateMu.RLock()
	window := ui.window
	ui.stateMu.RUnlock()
	if window == nil {
		return
	}
	window.Synchronize(func() {
		if ui.app.isClosing() || ui.window == nil {
			return
		}
		switch {
		case arguments == "show-status":
			ui.showStatus()
		case strings.HasPrefix(arguments, "copy-code:"):
			value, err := decodeToastValue(strings.TrimPrefix(arguments, "copy-code:"))
			if err == nil {
				ui.copyText("验证码", value)
			}
		case strings.HasPrefix(arguments, "copy-full:"):
			value, err := decodeToastValue(strings.TrimPrefix(arguments, "copy-full:"))
			if err == nil {
				ui.copyText("短信全文", value)
			}
		}
	})
}

// copyText copies on the UI thread. Success is confirmed in the status bar
// only, never with a dialog or another notification.
func (ui *desktopUI) copyText(label, value string) {
	if err := walk.Clipboard().SetText(value); err != nil {
		log.Printf("copy %s failed: %v", label, err)
		walk.MsgBox(ui.window, appName, "复制失败："+err.Error(), walk.MsgBoxIconError)
		return
	}
	ui.copyNotice = "已复制" + label
	ui.copyNoticeAt = time.Now()
	ui.refreshStatusItem()
}

// Clipboard access belongs to Walk's GUI thread; copying never opens another
// notification. Re-check age when dequeued after a busy UI.
func (ui *desktopUI) enqueueVerificationCode(sms SMS, code string) {
	ui.stateMu.RLock()
	defer ui.stateMu.RUnlock()
	if ui.window == nil || ui.app.isClosing() {
		return
	}
	ui.window.Synchronize(func() {
		if ui.app.isClosing() || !freshNotification(sms, time.Now()) {
			return
		}
		if err := walk.Clipboard().SetText(code); err != nil {
			log.Printf("automatic verification-code copy failed: %v", err)
		}
	})
}

func acquireSingleInstance() (windows.Handle, bool, error) {
	name, err := windows.UTF16PtrFromString(singleInstanceName)
	if err != nil {
		return 0, false, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return handle, true, nil
	}
	if err != nil {
		return handle, false, err
	}
	return handle, false, nil
}

func releaseSingleInstance(handle windows.Handle) {
	if handle != 0 {
		_ = windows.CloseHandle(handle)
	}
}

// showInstanceEventName is an auto-reset event the running instance waits on.
// A second launch sets it so the existing window comes to the front instead of
// leaving the user with "already running" and no visible window.
const showInstanceEventName = singleInstanceName + `.Show`

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procIsIconic                 = user32.NewProc("IsIconic")
	procShowWindow               = user32.NewProc("ShowWindow")
)

const swRestore = 9 // SW_RESTORE

// signalRunningInstance asks the running instance to show its window and
// reports whether an instance that listens exists; v0.7.6 and earlier have no
// event. With show false (auto-start) it only checks and leaves the window.
func signalRunningInstance(show bool) bool {
	name, err := windows.UTF16PtrFromString(showInstanceEventName)
	if err != nil {
		return false
	}
	event, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(event)
	if !show {
		return true
	}
	// Let the running instance take the foreground; ASFW_ANY is (DWORD)-1.
	_, _, _ = procAllowSetForegroundWindow.Call(uintptr(^uint32(0)))
	return windows.SetEvent(event) == nil
}

func createShowInstanceEvent() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(showInstanceEventName)
	if err != nil {
		return 0, err
	}
	return windows.CreateEvent(nil, 0, 0, name)
}

func showAlreadyRunning() {
	walk.MsgBox(nil, appName, "MsgDock 已经在运行，可能是旧版本。\r\n\r\n"+
		"它的图标在任务栏右下角，可能收在“^”隐藏图标里。\r\n"+
		"要换成这个版本，请先右键托盘图标选“退出”；找不到图标时，在任务管理器里结束 MsgDock，再重新打开。",
		walk.MsgBoxIconInformation)
}

func showFatalError(title string, err error) {
	walk.MsgBox(nil, appName, title+"：\r\n"+err.Error(), walk.MsgBoxIconError)
}
