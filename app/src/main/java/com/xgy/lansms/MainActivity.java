package com.xgy.lansms;

import android.Manifest;
import android.app.*;
import android.content.*;
import android.content.pm.PackageManager;
import android.content.res.ColorStateList;
import android.graphics.Color;
import android.net.Uri;
import android.os.*;
import android.provider.Settings;
import android.text.InputType;
import android.view.LayoutInflater;
import android.view.View;
import android.widget.*;
import java.net.*;
import java.nio.charset.StandardCharsets;
import java.util.*;

/**
 * Status-first home screen (DESIGN.md "状态优先的界面"). One Activity, one ScrollView,
 * sections in a fixed order; every collapse is visibility only and lives in the
 * instance state. All sync logic stays in the stores and services; this class only
 * gathers facts for {@link SyncState} and renders the result.
 */
public class MainActivity extends Activity {
    private static final int REQUEST_SMS = 10;
    private static final int REQUEST_NOTIFICATIONS = 11;
    private static final int REQUEST_NEARBY = 12;
    private static final int REQUEST_SCAN = 13;
    private static final int RECENT_ROWS = 5;
    private int pendingPermission;
    private boolean scanning;
    private Thread scanThread;

    // Status card and needs-action list
    private View statusDot;
    private TextView statusTitle, statusReason, statusRecent;
    private Button statusAction, statusLink;
    private View issuesCard;
    private LinearLayout issuesContainer;
    private String statusKey = "";

    // Recent SMS
    private LinearLayout recentContainer;
    private View recentHint, recentAll;
    private String recentStamp = "";

    // Receivers
    private LinearLayout receiversContainer;
    private View addReceiverContainer, nearbyPermissionRow;
    private Button addReceiverToggle;
    private TextView nearbyPermissionText;
    private boolean addReceiverExpanded;

    // Account
    private TextView accountStatusText;
    private Button accountToggle;
    private View accountBody, loginGroup, sessionGroup;
    private EditText accountUsernameEdit, accountEmailEdit, accountPasswordEdit;
    private CheckBox accountReceiveCheck;
    private boolean accountExpanded;

    // Advanced
    private View advancedContainer;
    private Button advancedToggle;
    private TextView relayUrlEdit, machineCodeText, backupStatusText, smsPermissionText, receiverStatusText;
    private TextView detailReceiverText, cloudStatusText, accountDetailText;
    private boolean advancedExpanded;

    private final Handler uiHandler = new Handler(Looper.getMainLooper());
    private final Runnable periodicRefresh = new Runnable() {
        @Override public void run() {
            if (!activityAlive()) return;
            renderAccount();
            renderStatus();
            renderRecentIfChanged();
            if (advancedExpanded) renderDetails();
            uiHandler.postDelayed(this, 3000L);
        }
    };

    private final List<TargetStore.Target> discovered = Collections.synchronizedList(new ArrayList<>());

    @Override public void onCreate(Bundle b) {
        super.onCreate(b);
        if (b != null) {
            pendingPermission = b.getInt("pending_permission", 0);
            advancedExpanded = b.getBoolean("advanced_expanded", false);
            accountExpanded = b.getBoolean("account_expanded", false);
            addReceiverExpanded = b.getBoolean("add_receiver_expanded", false);
        }
        setContentView(R.layout.activity_main);
        WindowLayout.apply(this);

        Notifications.ensureChannels(this);
        ReceiverService.migrateReceiverEnabled(this);
        ReceiverService.ensureStarted(this);
        CloudSyncJobService.schedule(this);

        initViews();
        setupListeners();
        render();

        DeviceBackupManager.onAppStarted(this, (success, message) -> {
            if (!activityAlive()) return;
            if (!success && message != null && message.contains("重新登记")) {
                Toast.makeText(this, message, Toast.LENGTH_LONG).show();
            }
            runOnUiThread(this::render);
        });
    }

    @Override protected void onResume() {
        super.onResume();
        ReceiverService.ensureStarted(this);
        render();
        uiHandler.post(periodicRefresh);
    }

    @Override protected void onPause() {
        uiHandler.removeCallbacks(periodicRefresh);
        super.onPause();
    }

    @Override protected void onSaveInstanceState(Bundle state) {
        state.putInt("pending_permission", pendingPermission);
        state.putBoolean("advanced_expanded", advancedExpanded);
        state.putBoolean("account_expanded", accountExpanded);
        state.putBoolean("add_receiver_expanded", addReceiverExpanded);
        super.onSaveInstanceState(state);
    }

    @Override protected void onDestroy() {
        uiHandler.removeCallbacks(periodicRefresh);
        if (scanThread != null) scanThread.interrupt();
        super.onDestroy();
    }

    private void askPermission(String permission, int requestCode) {
        if (checkSelfPermission(permission) == PackageManager.PERMISSION_GRANTED) {
            render();
            return;
        }
        if (pendingPermission != 0) return;
        pendingPermission = requestCode;
        requestPermissions(new String[]{permission}, requestCode);
    }

    @Override public void onRequestPermissionsResult(int requestCode, String[] permissions, int[] results) {
        super.onRequestPermissionsResult(requestCode, permissions, results);
        if (requestCode != pendingPermission) return;
        pendingPermission = 0;
        if (!activityAlive()) return;
        if (results.length == 0) {
            Toast.makeText(this, "已取消权限申请，请在需要时重试", Toast.LENGTH_SHORT).show();
            render();
            return;
        }
        boolean granted = results[0] == PackageManager.PERMISSION_GRANTED;
        if (requestCode == REQUEST_NOTIFICATIONS) {
            // Notification denial must not prevent receiving or saving history.
            startReceiverNow();
            if (!granted) Toast.makeText(this, "通知未开启，短信仍会保存在本机历史；可在应用设置中开启通知", Toast.LENGTH_LONG).show();
        } else if (requestCode == REQUEST_SCAN && granted) {
            scanLan();
        } else if (!granted) {
            String permission = requestCode == REQUEST_SMS ? Manifest.permission.RECEIVE_SMS : Manifest.permission.NEARBY_WIFI_DEVICES;
            String explanation = requestCode == REQUEST_SMS
                ? "短信权限未允许，无法监听本机新短信；接收其他设备的短信不受影响。"
                : "附近设备权限未允许，本次未开始扫描。也可以手动添加接收端 IP。";
            AlertDialog.Builder dialog = new AlertDialog.Builder(this).setTitle("权限未开启")
                .setMessage(explanation).setNegativeButton("知道了", null);
            if (!shouldShowRequestPermissionRationale(permission)) {
                dialog.setPositiveButton("应用设置", (d, w) -> openAppSettings());
            }
            dialog.show();
        }
        render();
    }

    private void openAppSettings() {
        startActivity(new Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
            Uri.parse("package:" + getPackageName())));
    }

    private void initViews() {
        statusDot = findViewById(R.id.view_status_dot);
        statusTitle = findViewById(R.id.text_status_title);
        statusReason = findViewById(R.id.text_status_reason);
        statusRecent = findViewById(R.id.text_status_recent);
        statusAction = findViewById(R.id.btn_status_action);
        statusLink = findViewById(R.id.btn_status_link);
        issuesCard = findViewById(R.id.card_issues);
        issuesContainer = findViewById(R.id.container_issues);

        recentContainer = findViewById(R.id.container_recent);
        recentHint = findViewById(R.id.text_recent_hint);
        recentAll = findViewById(R.id.btn_account_inbox);

        receiversContainer = findViewById(R.id.container_receivers);
        addReceiverContainer = findViewById(R.id.container_add_receiver);
        addReceiverToggle = findViewById(R.id.btn_toggle_add_receiver);
        nearbyPermissionRow = findViewById(R.id.row_nearby_permission);
        nearbyPermissionText = findViewById(R.id.text_nearby_permission);

        accountStatusText = findViewById(R.id.text_account_status);
        accountToggle = findViewById(R.id.btn_toggle_account);
        accountBody = findViewById(R.id.container_account_body);
        loginGroup = findViewById(R.id.group_account_login);
        sessionGroup = findViewById(R.id.group_account_session);
        accountUsernameEdit = findViewById(R.id.edit_account_username);
        accountEmailEdit = findViewById(R.id.edit_account_email);
        accountPasswordEdit = findViewById(R.id.edit_account_password);
        accountReceiveCheck = findViewById(R.id.check_account_receive);
        accountReceiveCheck.setChecked(AccountStore.receiveEnabled(this));

        advancedContainer = findViewById(R.id.container_advanced);
        advancedToggle = findViewById(R.id.btn_toggle_advanced);
        relayUrlEdit = findViewById(R.id.edit_relay_url);
        machineCodeText = findViewById(R.id.text_machine_code);
        backupStatusText = findViewById(R.id.text_backup_status);
        smsPermissionText = findViewById(R.id.text_sms_permission);
        receiverStatusText = findViewById(R.id.text_receiver_status);
        detailReceiverText = findViewById(R.id.text_detail_receiver);
        cloudStatusText = findViewById(R.id.text_cloud_status);
        accountDetailText = findViewById(R.id.text_account_detail);
    }

    private void setupListeners() {
        addReceiverToggle.setOnClickListener(v -> {
            addReceiverExpanded = !addReceiverExpanded;
            renderReceivers();
        });
        accountToggle.setOnClickListener(v -> {
            accountExpanded = !accountExpanded;
            renderAccount();
            if (accountExpanded && !AccountStore.hasAccount(this)) accountUsernameEdit.requestFocus();
        });
        advancedToggle.setOnClickListener(v -> {
            advancedExpanded = !advancedExpanded;
            renderAdvanced();
        });

        findViewById(R.id.btn_account_inbox).setOnClickListener(v -> showInbox());
        statusLink.setOnClickListener(v -> openBackgroundGuide());
        findViewById(R.id.btn_scan_lan).setOnClickListener(v -> scanLan());
        findViewById(R.id.btn_manual_add).setOnClickListener(v -> manualAdd());
        findViewById(R.id.btn_cloud_pair_sender).setOnClickListener(v -> cloudPairSender());
        findViewById(R.id.btn_send_test).setOnClickListener(v -> sendTest());
        findViewById(R.id.btn_request_nearby).setOnClickListener(v ->
            askPermission(Manifest.permission.NEARBY_WIFI_DEVICES, REQUEST_NEARBY));

        findViewById(R.id.btn_account_login).setOnClickListener(v -> accountLogin());
        findViewById(R.id.btn_account_register).setOnClickListener(v -> accountRegister());
        findViewById(R.id.btn_account_logout).setOnClickListener(v -> accountLogout());
        accountReceiveCheck.setOnCheckedChangeListener((button, enabled) -> {
            AccountStore.setReceiveEnabled(this, enabled);
            if (enabled && AccountStore.hasAccount(this)) startReceiver();
            render();
        });
        findViewById(R.id.btn_account_refresh).setOnClickListener(v -> {
            if (!AccountStore.hasAccount(this)) { Toast.makeText(this, "请先登录账号", Toast.LENGTH_SHORT).show(); return; }
            if (accountReceiveCheck.isChecked()) startReceiver();
            else accountReceiveCheck.setChecked(true); // The change listener starts it once.
            CloudRelay.executor().execute(() -> {
                boolean ok = AccountApi.pollInbox(getApplicationContext());
                runOnUiThread(() -> {
                    if (!activityAlive()) return;
                    Toast.makeText(this, ok ? "已收取，后台会继续补齐" : "暂未连接，将自动重试", Toast.LENGTH_SHORT).show();
                    render();
                    showInbox();
                });
            });
        });

        findViewById(R.id.btn_cloud_pair_receiver).setOnClickListener(v -> cloudPairReceiver());
        findViewById(R.id.btn_copy_machine_code).setOnClickListener(v -> copyMachineCode());
        findViewById(R.id.btn_sync_recover).setOnClickListener(v -> syncOrRecover());
        findViewById(R.id.btn_delete_device).setOnClickListener(v -> confirmDeleteDevice());
        findViewById(R.id.btn_start_receiver).setOnClickListener(v -> startReceiver());
        findViewById(R.id.btn_stop_receiver).setOnClickListener(v -> stopReceiver());
        findViewById(R.id.btn_request_sms).setOnClickListener(v ->
            askPermission(Manifest.permission.RECEIVE_SMS, REQUEST_SMS));
        findViewById(R.id.btn_battery_optimize).setOnClickListener(v -> requestBatteryWhitelist());
        findViewById(R.id.btn_background_guide).setOnClickListener(v -> openBackgroundGuide());
        findViewById(R.id.btn_app_settings).setOnClickListener(v -> openAppSettings());
        findViewById(R.id.btn_open_shizuku).setOnClickListener(v -> openShizuku());
        findViewById(R.id.btn_copy_adb).setOnClickListener(v -> copyAdbCommands());
    }

    private void render() {
        renderStatus();
        renderRecentIfChanged();
        renderReceivers();
        renderAccount();
        renderAdvanced();
    }

    // ---------------------------------------------------------------- status card

    private SyncState.Input stateInput() {
        SyncState.Input in = new SyncState.Input();
        in.now = System.currentTimeMillis();
        in.online = CloudRelay.hasNetwork(this);
        in.loggedIn = AccountStore.hasAccount(this);
        in.authRequired = in.loggedIn && AccountStore.authRequired(this);
        in.failingSinceAt = SyncClock.failingSince();
        in.canReceiveSms = getPackageManager().hasSystemFeature(PackageManager.FEATURE_TELEPHONY);
        in.smsPermission = checkSelfPermission(Manifest.permission.RECEIVE_SMS) == PackageManager.PERMISSION_GRANTED;
        NotificationManager notifications = getSystemService(NotificationManager.class);
        in.notificationsEnabled = notifications == null || notifications.areNotificationsEnabled();
        PowerManager power = getSystemService(PowerManager.class);
        in.batteryExempt = power == null || power.isIgnoringBatteryOptimizations(getPackageName());
        in.accountReceive = AccountStore.receiveEnabled(this);
        in.receiverEnabled = TargetStore.prefs(this).getBoolean(ReceiverService.PREF_RECEIVER_ENABLED, false);
        in.lanTargets = TargetStore.load(this).size();
        in.cloudSenderLinks = CloudConfigStore.senderLinks(this).size();
        in.cloudReceiverLinks = CloudConfigStore.receiverLinks(this).size();
        in.deadLetters = Math.max(0, CloudOutboxStore.deadLetterCount(this));
        in.accountPending = in.loggedIn ? AccountOutboxStore.count(this) : 0;
        in.legacyPending = CloudOutboxStore.count(this);
        in.inFlight = SyncClock.inFlight();
        in.pollIntervalMs = SyncClock.pollIntervalMs();
        in.lastSuccessAt = SyncClock.lastSuccess(this);
        in.lanProblem = SyncClock.lanProblem(this);
        in.lastForwardedAt = SyncClock.lastForwardedAt(this);
        in.lastForwardedTo = SyncClock.lastForwardedTo(this);
        in.lastReceivedAt = SyncClock.lastReceivedAt(this);
        in.lastReceivedFrom = SyncClock.lastReceivedFrom(this);
        return in;
    }

    private void renderStatus() {
        SyncState.Result state = SyncState.evaluate(stateInput());
        StringBuilder key = new StringBuilder(state.kind.name()).append('|').append(state.reason).append('|').append(state.recent);
        for (SyncState.Item item : state.items) key.append('|').append(item.text);
        // Rebuilding rows every 3 s would steal focus and ripples from the buttons; skip when unchanged.
        if (key.toString().equals(statusKey)) return;
        statusKey = key.toString();

        statusTitle.setText(state.title());
        statusReason.setText(state.reason);
        statusRecent.setText(state.recent);
        statusDot.setBackgroundTintList(ColorStateList.valueOf(Color.parseColor(state.colorHex())));

        // The item the reason line describes gets its button here; the list below only
        // carries the others, so one problem is never shown twice on the first screen.
        SyncState.Item primary = state.primary;
        if (primary != null) {
            statusAction.setVisibility(View.VISIBLE);
            statusAction.setText(primary.button);
            statusAction.setOnClickListener(v -> runAction(primary.action));
            statusLink.setVisibility(primary.guideLink ? View.VISIBLE : View.GONE);
        } else {
            statusAction.setVisibility(View.GONE);
            statusAction.setOnClickListener(null);
            statusLink.setVisibility(View.GONE);
        }

        issuesContainer.removeAllViews();
        LayoutInflater inflater = LayoutInflater.from(this);
        List<SyncState.Item> remaining = state.remainingItems();
        for (SyncState.Item item : remaining) {
            View row = inflater.inflate(R.layout.item_issue, issuesContainer, false);
            ((TextView) row.findViewById(R.id.text_issue)).setText(item.text);
            Button action = row.findViewById(R.id.btn_issue_action);
            action.setText(item.button);
            action.setOnClickListener(v -> runAction(item.action));
            Button link = row.findViewById(R.id.btn_issue_link);
            link.setVisibility(item.guideLink ? View.VISIBLE : View.GONE);
            if (item.guideLink) link.setOnClickListener(v -> openBackgroundGuide());
            issuesContainer.addView(row);
        }
        issuesCard.setVisibility(remaining.isEmpty() ? View.GONE : View.VISIBLE);
    }

    private void openBackgroundGuide() {
        startActivity(new Intent(this, BackgroundGuideActivity.class));
    }

    /** Every needs-action button reuses an existing flow; nothing new is persisted here. */
    private void runAction(SyncState.Action action) {
        switch (action) {
            case RELOGIN:
                accountExpanded = true;
                renderAccount();
                scrollTo(R.id.card_account);
                accountPasswordEdit.requestFocus();
                break;
            case GRANT_SMS:
                askPermission(Manifest.permission.RECEIVE_SMS, REQUEST_SMS);
                break;
            case ENABLE_NOTIFICATIONS:
                if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                        != PackageManager.PERMISSION_GRANTED && shouldShowRequestPermissionRationale(Manifest.permission.POST_NOTIFICATIONS)) {
                    askPermission(Manifest.permission.POST_NOTIFICATIONS, REQUEST_NOTIFICATIONS);
                } else {
                    try {
                        startActivity(new Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                            .putExtra(Settings.EXTRA_APP_PACKAGE, getPackageName()));
                    } catch (ActivityNotFoundException e) { openAppSettings(); }
                }
                break;
            case BATTERY:
                requestBatteryWhitelist();
                break;
            case ADD_ROUTE:
                // Show the two choices side by side: the receivers card with its
                // "添加接收端" link, and directly under it the open login form. Opening
                // the add-receiver block too would push the form below the fold, and
                // focusing a field here would pull the scroll away on IME-eager devices.
                accountExpanded = true;
                renderAccount();
                scrollTo(getPackageManager().hasSystemFeature(PackageManager.FEATURE_TELEPHONY)
                    ? R.id.card_receivers : R.id.card_account); // a tablet only has the login choice
                break;
            case ENABLE_RECEIVE:
                // The checkbox listener saves the flag and starts the service once;
                // when it is already ticked only the service start is still missing.
                accountExpanded = true;
                renderAccount();
                scrollTo(R.id.card_account);
                if (accountReceiveCheck.isChecked()) startReceiver();
                else accountReceiveCheck.setChecked(true);
                break;
            case DEAD_LETTERS:
                showDeadLetters();
                break;
            default:
                break;
        }
    }

    private void scrollTo(int viewId) {
        View target = findViewById(viewId);
        ScrollView scroll = findViewById(R.id.main_scroll);
        if (target != null && scroll != null) scroll.post(() -> scroll.smoothScrollTo(0, target.getTop()));
    }

    /** Dead letters hold only ciphertext, so the list shows when, how often and why. */
    private void showDeadLetters() {
        CloudRelay.executor().execute(() -> {
            List<org.json.JSONObject> rows = CloudOutboxStore.deadLetters(getApplicationContext(), 50);
            runOnUiThread(() -> {
                if (!activityAlive()) return;
                if (rows.isEmpty()) {
                    Toast.makeText(this, "没有可显示的失败记录", Toast.LENGTH_SHORT).show();
                    return;
                }
                // A platform AlertDialog shows either a message or a list, so the rows go into one text.
                StringBuilder text = new StringBuilder(
                    "这些短信经配对云端多次发送失败，已停止重试。正文是加密的，这里只能显示时间和原因；账号和局域网路径不受影响。");
                for (org.json.JSONObject row : rows) {
                    String reason = row.optString("deadLetterReason", row.optString("lastError", ""));
                    text.append("\n\n").append(android.text.format.DateFormat.format("MM-dd HH:mm", row.optLong("deadLetterAt")))
                        .append(" · 尝试 ").append(row.optInt("attempts")).append(" 次\n")
                        .append(reason.length() > 120 ? reason.substring(0, 120) + "…" : reason);
                }
                new AlertDialog.Builder(this)
                    .setTitle(rows.size() + " 条短信多次发送失败")
                    .setMessage(text)
                    .setPositiveButton("知道了", null)
                    .show();
            });
        });
    }

    // ---------------------------------------------------------------- recent SMS

    /** Re-reads the inbox only when the history file actually changed (size or mtime). */
    private void renderRecentIfChanged() {
        java.io.File file = new java.io.File(getFilesDir(), "cloud-inbox.jsonl");
        String user = AccountStore.userId(this);
        boolean receiving = TargetStore.prefs(this).getBoolean(ReceiverService.PREF_RECEIVER_ENABLED, false);
        // The receiver flag only changes the empty-state wording, but it must still re-render.
        String stamp = user + "|" + receiving + "|" + file.length() + "|" + file.lastModified();
        if (stamp.equals(recentStamp)) return;
        recentStamp = stamp;
        CloudRelay.executor().execute(() -> {
            List<org.json.JSONObject> rows;
            try { rows = CloudInboxStore.localHistory(getFilesDir(), user, RECENT_ROWS); }
            catch (Exception e) { rows = Collections.emptyList(); }
            final List<org.json.JSONObject> recent = rows;
            runOnUiThread(() -> {
                if (activityAlive() && user.equals(AccountStore.userId(this))) renderRecent(recent, user);
            });
        });
    }

    private void renderRecent(List<org.json.JSONObject> rows, String user) {
        recentContainer.removeAllViews();
        recentHint.setVisibility(rows.isEmpty() ? View.GONE : View.VISIBLE);
        recentAll.setVisibility(rows.isEmpty() ? View.GONE : View.VISIBLE);
        if (rows.isEmpty()) {
            // The local inbox only holds what other devices sent here; a phone that
            // only forwards must not be told its own SMS will show up in this list.
            boolean receiving = TargetStore.prefs(this).getBoolean(ReceiverService.PREF_RECEIVER_ENABLED, false);
            TextView empty = new TextView(this);
            empty.setText(receiving
                ? "还没有收到短信。其他设备发来的短信会显示在这里，最新的在最上面。"
                : "这里只显示其他设备发来的短信；本机最近转发的一条在状态卡里。");
            empty.setTextAppearance(R.style.Text_Body);
            empty.setPadding(0, dp(8), 0, dp(8));
            recentContainer.addView(empty);
            return;
        }
        LayoutInflater inflater = LayoutInflater.from(this);
        for (org.json.JSONObject row : rows) {
            View item = inflater.inflate(R.layout.item_recent_sms, recentContainer, false);
            String body = row.optString("text", "");
            String from = row.optString("from", "短信");
            String device = row.optString("device", "");
            ((TextView) item.findViewById(R.id.text_recent_from)).setText(device.isEmpty() ? from : from + " · " + device);
            ((TextView) item.findViewById(R.id.text_recent_time)).setText(shortTime(row.optLong("receivedAt")));
            ((TextView) item.findViewById(R.id.text_recent_body)).setText(body.replace('\n', ' '));
            String code = OtpCode.find(body);
            TextView chip = item.findViewById(R.id.text_recent_code);
            if (!code.isEmpty()) {
                chip.setVisibility(View.VISIBLE);
                chip.setText(code);
                item.setContentDescription(from + "，验证码 " + code + "，点按复制");
                item.setOnClickListener(v -> copyInboxText(code, user, "已复制验证码"));
            } else {
                item.setOnClickListener(v -> showInboxDetail(row, user));
            }
            item.setOnLongClickListener(v -> {
                copyInboxText(body, user, "已复制全文");
                return true;
            });
            recentContainer.addView(item);
        }
    }

    /** Today → HH:mm; otherwise MM-dd HH:mm. */
    private static String shortTime(long at) {
        if (at <= 0) return "";
        Calendar now = Calendar.getInstance();
        Calendar then = Calendar.getInstance();
        then.setTimeInMillis(at);
        boolean sameDay = now.get(Calendar.YEAR) == then.get(Calendar.YEAR)
            && now.get(Calendar.DAY_OF_YEAR) == then.get(Calendar.DAY_OF_YEAR);
        return android.text.format.DateFormat.format(sameDay ? "HH:mm" : "MM-dd HH:mm", at).toString();
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }

    // ---------------------------------------------------------------- receivers

    /** LAN targets and cloud links in one list, each tagged; adding lives in a collapsed area. */
    private void renderReceivers() {
        receiversContainer.removeAllViews();
        List<TargetStore.Target> targets = TargetStore.load(this);
        List<CloudConfigStore.CloudLink> links = CloudConfigStore.loadLinks(this);
        LayoutInflater inflater = LayoutInflater.from(this);
        for (int i = 0; i < targets.size(); i++) {
            TargetStore.Target t = targets.get(i);
            View item = inflater.inflate(R.layout.item_receiver, receiversContainer, false);
            ((TextView) item.findViewById(R.id.text_receiver_name)).setText(t.name);
            ((TextView) item.findViewById(R.id.text_receiver_tag)).setText("局域网");
            ((TextView) item.findViewById(R.id.text_receiver_detail)).setText(t.host + ":" + t.port);
            final int idx = i;
            item.findViewById(R.id.btn_receiver_remove).setOnClickListener(v -> {
                List<TargetStore.Target> now = TargetStore.load(this);
                if (idx < now.size() && now.get(idx).host.equals(t.host) && now.get(idx).port == t.port) {
                    now.remove(idx);
                    TargetStore.save(this, now);
                }
                render();
            });
            receiversContainer.addView(item);
        }
        for (CloudConfigStore.CloudLink link : links) {
            View item = inflater.inflate(R.layout.item_receiver, receiversContainer, false);
            String name = link.peerName.isEmpty() ? link.peerDeviceId : link.peerName;
            ((TextView) item.findViewById(R.id.text_receiver_name)).setText(name);
            ((TextView) item.findViewById(R.id.text_receiver_tag)).setText("云端");
            ((TextView) item.findViewById(R.id.text_receiver_detail)).setText(
                (link.isSender() ? "本机发送到此设备" : "本机接收此设备的短信") + " · 端到端加密");
            item.findViewById(R.id.btn_receiver_remove).setOnClickListener(v -> confirmRemoveLink(link));
            receiversContainer.addView(item);
        }
        if (targets.isEmpty() && links.isEmpty()) {
            TextView empty = new TextView(this);
            empty.setText("还没有接收端。登录账号后短信会同步到网页和电脑；也可以添加局域网电脑或云端设备。");
            empty.setTextAppearance(R.style.Text_Body);
            empty.setPadding(0, dp(8), 0, dp(8));
            receiversContainer.addView(empty);
        }

        addReceiverContainer.setVisibility(addReceiverExpanded ? View.VISIBLE : View.GONE);
        addReceiverToggle.setText(addReceiverExpanded ? "收起" : "添加接收端");
        if (Build.VERSION.SDK_INT >= 33) {
            nearbyPermissionRow.setVisibility(View.VISIBLE);
            boolean nearby = checkSelfPermission(Manifest.permission.NEARBY_WIFI_DEVICES) == PackageManager.PERMISSION_GRANTED;
            nearbyPermissionText.setText("附近设备权限：" + (nearby ? "已允许" : "未允许（扫描需要）"));
            findViewById(R.id.btn_request_nearby).setVisibility(nearby ? View.GONE : View.VISIBLE);
        } else {
            nearbyPermissionRow.setVisibility(View.GONE);
        }
    }

    // ---------------------------------------------------------------- account

    /** One line until expanded; the form only while logged out or when a relogin is needed. */
    private void renderAccount() {
        boolean loggedIn = AccountStore.hasAccount(this);
        boolean relogin = loggedIn && AccountStore.authRequired(this);
        String name = AccountStore.username(this);
        if (name.isEmpty()) name = AccountStore.email(this);
        if (name.isEmpty()) name = "已登录账号";
        accountStatusText.setText(!loggedIn ? "账号：未登录"
            : relogin ? "账号：" + name + " · 需要重新登录" : "账号：已登录 " + name);
        accountToggle.setText(accountExpanded ? "收起" : loggedIn ? "管理" : "登录");

        accountBody.setVisibility(accountExpanded ? View.VISIBLE : View.GONE);
        loginGroup.setVisibility(!loggedIn || relogin ? View.VISIBLE : View.GONE);
        sessionGroup.setVisibility(loggedIn ? View.VISIBLE : View.GONE);
        findViewById(R.id.btn_account_refresh).setVisibility(relogin ? View.GONE : View.VISIBLE);
        if (relogin && accountUsernameEdit.getText().length() == 0) {
            String saved = AccountStore.username(this);
            accountUsernameEdit.setText(saved.isEmpty() ? AccountStore.email(this) : saved);
        }
        accountEmailEdit.setVisibility(loggedIn ? View.GONE : View.VISIBLE);
        findViewById(R.id.btn_account_register).setVisibility(loggedIn ? View.GONE : View.VISIBLE);
    }

    // ---------------------------------------------------------------- advanced

    private void renderAdvanced() {
        advancedContainer.setVisibility(advancedExpanded ? View.VISIBLE : View.GONE);
        advancedToggle.setText(advancedExpanded ? "收起高级设置" : "高级设置");
        if (!advancedExpanded) return;
        relayUrlEdit.setText(RelayHttp.PRIMARY);
        ((TextView) findViewById(R.id.text_backup_relay_url)).setText(RelayHttp.BACKUP);
        machineCodeText.setText(DeviceBackupManager.machineCode(this));
        boolean sms = checkSelfPermission(Manifest.permission.RECEIVE_SMS) == PackageManager.PERMISSION_GRANTED;
        smsPermissionText.setText("短信权限：" + (sms ? "已允许" : "未允许"));
        findViewById(R.id.btn_request_sms).setVisibility(sms ? View.GONE : View.VISIBLE);
        renderDetails();
    }

    /** The old multi-line diagnostics, verbatim, so nothing is lost behind the status card. */
    private void renderDetails() {
        boolean on = TargetStore.prefs(this).getBoolean(ReceiverService.PREF_RECEIVER_ENABLED, false);
        String status = on ? ReceiverService.statusText()
            : (ReceiverService.isRunningOrStarting() ? "正在停止…" : "未启动");
        receiverStatusText.setText("状态：" + status
            + "\n局域网配对码：" + TargetStore.ensurePairCode(this)
            + "\n地址：http://" + ReceiverService.localIpv4(this) + ":58123");
        detailReceiverText.setText(receiverDetail(status));
        cloudStatusText.setText(CloudRelay.statusText(this));
        accountDetailText.setText(AccountApi.statusText(this));
        backupStatusText.setText(DeviceBackupManager.statusText(this));
    }

    private String receiverDetail(String status) {
        String pending = CloudRelay.pendingReceiverCode(this);
        NotificationManager notifications = getSystemService(NotificationManager.class);
        boolean notify = notifications != null && notifications.areNotificationsEnabled();
        return "状态：" + status +
            "\n地址：http://" + ReceiverService.localIpv4(this) + ":58123" +
            "\n局域网配对码：" + TargetStore.ensurePairCode(this) +
            "\n云接收链路：" + CloudConfigStore.receiverLinks(this).size() + " 条" +
            (pending.isEmpty() ? "" : "\n云配对码（接收）：" + pending) +
            (notify ? "" : "\n通知未开启：短信仍保存到本机历史，可在应用设置开启通知");
    }

    // ---------------------------------------------------------------- cloud pairing

    private void cloudPairSender() {
        String relayUrl = RelayHttp.PRIMARY;
        EditText code = new EditText(this);
        code.setHint("接收端显示的 6 位云配对码");
        code.setInputType(InputType.TYPE_CLASS_NUMBER);

        new AlertDialog.Builder(this)
            .setTitle("输入云配对码")
            .setMessage("先在 Windows 或 Android 接收端生成云配对码，再在这里输入它显示的 6 位数字。")
            .setView(code)
            .setPositiveButton("配对", (d, w) -> {
                String value = code.getText().toString().trim();
                if (!value.matches("\\d{6}")) {
                    Toast.makeText(this, "云配对码应为 6 位数字", Toast.LENGTH_LONG).show();
                    return;
                }
                try {
                    CloudRelay.saveRelayUrl(this, relayUrl);
                } catch (Exception e) {
                    Toast.makeText(this, e.getMessage(), Toast.LENGTH_LONG).show();
                    return;
                }
                Toast.makeText(this, "正在配对…", Toast.LENGTH_SHORT).show();
                CloudRelay.pair(this, relayUrl, value, (success, message) -> runOnUiThread(() -> {
                    if (!activityAlive()) return;
                    Toast.makeText(this, message, Toast.LENGTH_LONG).show();
                    render();
                }));
            })
            .setNegativeButton("取消", null)
            .show();
    }

    private void cloudPairReceiver() {
        String relayUrl = RelayHttp.PRIMARY;
        try {
            CloudRelay.saveRelayUrl(this, relayUrl);
        } catch (Exception e) {
            Toast.makeText(this, e.getMessage(), Toast.LENGTH_LONG).show();
            return;
        }

        Toast.makeText(this, "正在生成云配对码…", Toast.LENGTH_SHORT).show();
        CloudRelay.startReceiverPairing(this, relayUrl, new CloudRelay.ReceiverPairCallback() {
            @Override public void started(String code) {
                runOnUiThread(() -> {
                    if (!activityAlive()) return;
                    new AlertDialog.Builder(MainActivity.this)
                        .setTitle("云配对码")
                        .setMessage("请在发送端手机的“添加接收端 → 输入云配对码”里输入：\n\n" + code +
                            "\n\n保持本页或接收服务运行，配对完成后会自动保存并开始接收。")
                        .setPositiveButton("知道了", null)
                        .show();
                });
            }
            @Override public void completed(boolean success, String message) {
                runOnUiThread(() -> {
                    if (!activityAlive()) return;
                    Toast.makeText(MainActivity.this, message, Toast.LENGTH_LONG).show();
                    if (success) startReceiver();
                    else render();
                });
            }
        });
    }

    // ---------------------------------------------------------------- account actions

    private void accountLogin() {
        setAccountBusy(true);
        String username = accountUsernameEdit.getText().toString().trim();
        String email = accountEmailEdit.getText().toString().trim();
        String password = accountPasswordEdit.getText().toString();
        String identifier = username.isEmpty() ? email : username;
        Toast.makeText(this, "正在登录 MsgDock…", Toast.LENGTH_SHORT).show();
        AccountApi.login(this, identifier, password, (success, message) -> {
            if (!activityAlive()) return;
            finishAccountLogin(success);
            Toast.makeText(this, message, Toast.LENGTH_LONG).show();
            render();
        });
    }

    private void accountRegister() {
        setAccountBusy(true);
        String username = accountUsernameEdit.getText().toString().trim();
        String email = accountEmailEdit.getText().toString().trim();
        String password = accountPasswordEdit.getText().toString();
        Toast.makeText(this, "正在注册 MsgDock…", Toast.LENGTH_SHORT).show();
        AccountApi.register(this, username, email, password, (success, message) -> {
            if (!activityAlive()) return;
            finishAccountLogin(success);
            Toast.makeText(this, message, Toast.LENGTH_LONG).show();
            render();
        });
    }

    private void accountLogout() {
        setAccountBusy(true);
        AccountApi.logout(this, (success, message) -> {
            if (!activityAlive()) return;
            setAccountBusy(false);
            accountReceiveCheck.setChecked(false);
            Toast.makeText(this, message, Toast.LENGTH_LONG).show();
            render();
        });
    }

    private void setAccountBusy(boolean busy) {
        findViewById(R.id.btn_account_login).setEnabled(!busy);
        findViewById(R.id.btn_account_register).setEnabled(!busy);
        findViewById(R.id.btn_account_logout).setEnabled(!busy);
    }

    private void finishAccountLogin(boolean success) {
        setAccountBusy(false);
        if (success) {
            accountPasswordEdit.setText("");
            if (AccountStore.receiveEnabled(this)) startReceiver();
        }
    }

    // ---------------------------------------------------------------- inbox

    private void showInbox() {
        String user = AccountStore.userId(this);
        CloudRelay.executor().execute(() -> {
            try {
                List<org.json.JSONObject> rows = CloudInboxStore.localHistory(getFilesDir(), user, 200);
                runOnUiThread(() -> {
                    if (!activityAlive() || !user.equals(AccountStore.userId(this))) return;
                    if (rows.isEmpty()) {
                        new AlertDialog.Builder(this).setTitle("本机收件箱")
                            .setMessage("暂无已保存的短信。局域网和配对云端收到的短信无需登录即可查看；登录后也会显示当前账号已同步到本机的历史。")
                            .setPositiveButton("知道了", null).show();
                        return;
                    }
                    String[] labels = new String[rows.size()];
                    for (int i = 0; i < rows.size(); i++) {
                        org.json.JSONObject row = rows.get(i);
                        String body = row.optString("text", "").replace('\n', ' ');
                        labels[i] = row.optString("from") + "\n" + inboxMetadata(row) + "\n"
                                + (body.length() > 60 ? body.substring(0, 60) + "…" : body);
                    }
                    new AlertDialog.Builder(this).setTitle("本机收件箱（最近 " + rows.size() + " 条）")
                        .setItems(labels, (dialog, which) -> showInboxDetail(rows.get(which), user))
                        .setNegativeButton("关闭", null).show();
                });
            } catch (Exception e) {
                runOnUiThread(() -> { if (activityAlive()) Toast.makeText(this, "读取收件箱失败，请重试", Toast.LENGTH_LONG).show(); });
            }
        });
    }

    private void showInboxDetail(org.json.JSONObject row, String user) {
        if (!activityAlive() || !user.equals(AccountStore.userId(this))) return;
        String body = row.optString("text");
        AlertDialog.Builder detail = new AlertDialog.Builder(this).setTitle(row.optString("from"))
            .setMessage(inboxMetadata(row) + "\n\n" + body)
            .setPositiveButton("复制全文", (d, w) -> copyInboxText(body, user, "已复制全文")).setNegativeButton("关闭", null);
        // Prefer an explicit OTP; fall back to the legacy digit rule so manual copy still works.
        String value = OtpCode.find(body);
        if (value.isEmpty()) {
            java.util.regex.Matcher code = java.util.regex.Pattern.compile("(?<!\\d)(\\d{4,8})(?!\\d)").matcher(body);
            if (code.find()) value = code.group(1);
        }
        if (!value.isEmpty()) {
            String copied = value;
            detail.setNeutralButton("复制 " + copied, (d, w) -> copyInboxText(copied, user, "已复制验证码"));
        }
        detail.show();
    }

    private String inboxMetadata(org.json.JSONObject row) {
        String source = row.optString("source");
        String label = "lan".equals(source) ? "局域网" : "cloud".equals(source) ? "配对云端" : "账号同步";
        return row.optString("device", "未知设备") + " · " + label + " · "
            + java.text.DateFormat.getDateTimeInstance().format(new Date(row.optLong("receivedAt")));
    }

    /** SMS bodies and codes are sensitive: keep them out of the system copy preview. */
    private void copyInboxText(String text, String user, String confirmation) {
        if (!activityAlive() || !user.equals(AccountStore.userId(this))) return;
        if (OtpCopyReceiver.copy(this, text)) {
            if (Build.VERSION.SDK_INT < 33) Toast.makeText(this, confirmation, Toast.LENGTH_SHORT).show();
        } else {
            Toast.makeText(this, "复制失败，请重试", Toast.LENGTH_SHORT).show();
        }
    }

    // ---------------------------------------------------------------- device backup

    private void copyMachineCode() {
        String code = DeviceBackupManager.machineCode(this);
        if (code.contains("未登记") || code.contains("计算中") || code.contains("不可用")) {
            Toast.makeText(this, "设备编号仍不可用，请稍后再试", Toast.LENGTH_LONG).show();
            return;
        }
        ((ClipboardManager)getSystemService(CLIPBOARD_SERVICE))
            .setPrimaryClip(ClipData.newPlainText("Xgy 机器码", code));
        Toast.makeText(this, "机器码已复制", Toast.LENGTH_SHORT).show();
    }

    private void syncOrRecover() {
        Toast.makeText(this, "正在按机器码检查云端链路…", Toast.LENGTH_SHORT).show();
        DeviceBackupManager.syncOrRecover(this, (ok, message) -> {
            if (!activityAlive()) return;
            Toast.makeText(this, message, Toast.LENGTH_LONG).show();
            render();
        });
    }

    private void confirmRemoveLink(CloudConfigStore.CloudLink link) {
        String name = link.peerName.isEmpty() ? link.peerDeviceId : link.peerName;
        new AlertDialog.Builder(this)
            .setTitle("移除云端设备？")
            .setMessage("将撤销与“" + name + "”的云端链路，并清理关联待发送消息。此操作不可自动恢复。")
            .setNegativeButton("取消", null)
            .setPositiveButton("确认移除", (d, w) -> {
                Toast.makeText(this, "正在撤销云链路…", Toast.LENGTH_SHORT).show();
                DeviceBackupManager.removeLink(this, link, (ok, message) -> {
                    if (!activityAlive()) return;
                    Toast.makeText(this, message, Toast.LENGTH_LONG).show();
                    render();
                });
            })
            .show();
    }

    private void confirmDeleteDevice() {
        new AlertDialog.Builder(this)
            .setTitle("删除云端机器记录？")
            .setMessage("将撤销本机全部云链路、删除加密备份并清空本机云链路。之后不会自动重新上传，必须手动重新登记。")
            .setNegativeButton("取消", null)
            .setPositiveButton("确认删除全部", (d, w) -> {
                Toast.makeText(this, "正在删除云端机器记录…", Toast.LENGTH_SHORT).show();
                DeviceBackupManager.deleteDevice(this, (ok, message) -> {
                    if (!activityAlive()) return;
                    Toast.makeText(this, message, Toast.LENGTH_LONG).show();
                    render();
                });
            })
            .show();
    }

    // ---------------------------------------------------------------- receiver service

    private void startReceiver() {
        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            askPermission(Manifest.permission.POST_NOTIFICATIONS, REQUEST_NOTIFICATIONS);
            return;
        }
        startReceiverNow();
    }

    private void startReceiverNow() {
        if (!TargetStore.prefs(this).edit().putBoolean(ReceiverService.PREF_RECEIVER_ENABLED, true).commit()) {
            Toast.makeText(this, "启动设置保存失败，请重试", Toast.LENGTH_LONG).show();
            return;
        }
        boolean requested = ReceiverService.ensureStarted(this);
        Toast.makeText(this, requested ? ReceiverService.statusText() : "启动失败，请重试或检查后台限制", Toast.LENGTH_LONG).show();
        render();
    }

    private void stopReceiver() {
        if (!TargetStore.prefs(this).edit().putBoolean(ReceiverService.PREF_RECEIVER_ENABLED, false).commit()) {
            Toast.makeText(this, "停止设置保存失败，请重试", Toast.LENGTH_LONG).show();
            return;
        }
        BootReceiver.cancelReceiverRestart(this);
        if (ReceiverService.isRunningOrStarting()) {
            Intent stop = new Intent(this, ReceiverService.class).setAction(ReceiverService.ACTION_STOP);
            try {
                if (Build.VERSION.SDK_INT >= 26) startForegroundService(stop);
                else startService(stop);
            } catch (RuntimeException e) {
                stopService(new Intent(this, ReceiverService.class));
            }
        }
        render();
    }

    // ---------------------------------------------------------------- LAN targets

    private void scanLan() {
        if (scanning) return;
        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.NEARBY_WIFI_DEVICES) != PackageManager.PERMISSION_GRANTED) {
            askPermission(Manifest.permission.NEARBY_WIFI_DEVICES, REQUEST_SCAN);
            return;
        }
        scanning = true;
        Button scanButton = findViewById(R.id.btn_scan_lan);
        scanButton.setEnabled(false);
        scanButton.setText("正在扫描…");
        Toast.makeText(this, "正在扫描约 5 秒…", Toast.LENGTH_SHORT).show();
        discovered.clear();

        scanThread = new Thread(() -> {
            long end = SystemClock.elapsedRealtime() + 5200;
            String failure = null;
            Set<String> seen = new HashSet<>();
            try (DatagramSocket ds = new DatagramSocket(null)) {
                ds.setReuseAddress(true);
                ds.bind(new InetSocketAddress(ReceiverService.DISCOVERY_PORT));
                LanNet.bindWifi(this, ds);
                ds.setSoTimeout(600);
                byte[] buf = new byte[1024];

                while (!Thread.currentThread().isInterrupted() && SystemClock.elapsedRealtime() < end) {
                    try {
                        DatagramPacket p = new DatagramPacket(buf, buf.length);
                        ds.receive(p);
                        String s = new String(p.getData(), p.getOffset(), p.getLength(), StandardCharsets.UTF_8);
                        String[] f = s.split("\\|", 4);
                        if (f.length == 4 && "XGY_SMS_V1".equals(f[0])) {
                            int port;
                            try { port = Integer.parseInt(f[3]); } catch (NumberFormatException invalidPacket) { continue; }
                            if (port < 1 || port > 65535) continue;
                            String key = f[2] + ":" + f[3];
                            if (seen.add(key)) {
                                discovered.add(new TargetStore.Target(f[1], f[2], port, ""));
                            }
                        }
                    } catch (SocketTimeoutException ignored) {}
                }
            } catch (Exception e) {
                failure = "扫描失败，请检查 Wi-Fi 后重试，也可以手动添加接收端 IP。";
                android.util.Log.w("XgyLanSms", "LAN discovery failed", e);
            }
            if (Thread.currentThread().isInterrupted()) return;
            final String error = failure;
            runOnUiThread(() -> {
                if (!activityAlive()) return;
                scanning = false;
                scanThread = null;
                scanButton.setEnabled(true);
                scanButton.setText("扫描局域网");
                if (error != null) new AlertDialog.Builder(this).setTitle("无法扫描")
                    .setMessage(error).setPositiveButton("知道了", null).show();
                else showDiscovered();
            });
        }, "MsgDock-LAN-scan");
        scanThread.start();
    }

    private void showDiscovered() {
        if (discovered.isEmpty()) {
            new AlertDialog.Builder(this)
                .setTitle("未发现设备")
                .setMessage("请确认 Windows 或 Android 接收端正在运行且处于同一 Wi‑Fi。也可以使用“手动添加”。")
                .setPositiveButton("知道了", null)
                .show();
            return;
        }

        String[] labels = new String[discovered.size()];
        for (int i = 0; i < labels.length; i++) {
            labels[i] = discovered.get(i).label();
        }
        new AlertDialog.Builder(this)
            .setTitle("选择接收端")
            .setItems(labels, (d, which) -> askPairCode(discovered.get(which)))
            .show();
    }

    private void askPairCode(TargetStore.Target base) {
        EditText code = new EditText(this);
        code.setHint("接收端显示的 6 位局域网配对码");
        code.setInputType(InputType.TYPE_CLASS_NUMBER);

        new AlertDialog.Builder(this)
            .setTitle("配对 " + base.name)
            .setView(code)
            .setPositiveButton("保存", (d, w) -> {
                String c = code.getText().toString().trim();
                if (c.length() != 6) {
                    Toast.makeText(this, "局域网配对码应为 6 位", Toast.LENGTH_LONG).show();
                    return;
                }
                List<TargetStore.Target> ts = TargetStore.load(this);
                ts.add(new TargetStore.Target(base.name, base.host, base.port, c));
                TargetStore.save(this, ts);
                render();
            })
            .setNegativeButton("取消", null)
            .show();
    }

    private void manualAdd() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        int p = dp(16);
        box.setPadding(p, p, p, p);

        EditText name = new EditText(this);
        name.setHint("名称，例如 Office-PC");
        EditText host = new EditText(this);
        host.setHint("IP，例如 192.168.1.20");
        EditText code = new EditText(this);
        code.setHint("6 位局域网配对码");
        code.setInputType(InputType.TYPE_CLASS_NUMBER);

        box.addView(name);
        box.addView(host);
        box.addView(code);

        new AlertDialog.Builder(this)
            .setTitle("手动添加局域网电脑")
            .setView(box)
            .setPositiveButton("保存", (d, w) -> {
                if (host.getText().toString().trim().isEmpty() ||
                    code.getText().toString().trim().length() != 6) {
                    Toast.makeText(this, "请填写 IP 和 6 位局域网配对码", Toast.LENGTH_LONG).show();
                    return;
                }
                List<TargetStore.Target> ts = TargetStore.load(this);
                String targetName = name.getText().toString().trim().isEmpty() ?
                    "Manual" : name.getText().toString().trim();
                ts.add(new TargetStore.Target(targetName, host.getText().toString().trim(), 58123,
                    code.getText().toString().trim()));
                TargetStore.save(this, ts);
                render();
            })
            .setNegativeButton("取消", null)
            .show();
    }

    private void sendTest() {
        List<TargetStore.Target> ts = TargetStore.load(this);
        if (ts.isEmpty() && !CloudConfigStore.isConfigured(CloudConfigStore.load(this))) {
            Toast.makeText(this, "请先添加局域网电脑或输入云配对码", Toast.LENGTH_LONG).show();
            return;
        }
        Forwarder.forwardAsync(this, UUID.randomUUID().toString(), "MsgDock",
            "测试消息：局域网/云端短信转发已连通。验证码 123456", System.currentTimeMillis(), -1);
        Toast.makeText(this, "已发送测试消息（局域网与云端共用同一 ID）", Toast.LENGTH_SHORT).show();
    }

    // ---------------------------------------------------------------- system shortcuts

    /** Direct whitelist prompt first, then the system list, then app info; never a crash. */
    private void requestBatteryWhitelist() {
        try {
            Intent i = new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                Uri.parse("package:" + getPackageName()));
            startActivity(i);
        } catch (Exception e) {
            try {
                startActivity(new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS));
            } catch (Exception ignored) {
                openAppSettings();
            }
        }
    }

    private void openShizuku() {
        try {
            Intent i = getPackageManager().getLaunchIntentForPackage("moe.shizuku.privileged.api");
            if (i != null) startActivity(i);
            else Toast.makeText(this, "未检测到 Shizuku", Toast.LENGTH_LONG).show();
        } catch (Exception e) {
            Toast.makeText(this, "无法打开 Shizuku", Toast.LENGTH_LONG).show();
        }
    }

    private void copyAdbCommands() {
        String pkg = getPackageName();
        String cmd = "adb shell pm grant " + pkg + " android.permission.RECEIVE_SMS\n" +
            "adb shell pm grant " + pkg + " android.permission.NEARBY_WIFI_DEVICES\n" +
            "adb shell cmd appops set " + pkg + " RUN_IN_BACKGROUND allow\n" +
            "adb shell cmd appops set " + pkg + " RUN_ANY_IN_BACKGROUND allow\n" +
            "adb shell dumpsys deviceidle whitelist +" + pkg;
        ((ClipboardManager)getSystemService(CLIPBOARD_SERVICE))
            .setPrimaryClip(ClipData.newPlainText("ADB/Shizuku", cmd));

        new AlertDialog.Builder(this)
            .setTitle("已复制")
            .setMessage(cmd + "\n\n可在电脑 ADB 中执行；使用 Shizuku 时，可在 rish/支持 Shizuku 的终端中去掉每行开头的 `adb shell ` 后执行。" +
                "注意：这只能补权限/后台策略，无法绕过 HyperOS 在短信广播之前的系统级过滤。")
            .setPositiveButton("确定", null)
            .show();
    }

    private boolean activityAlive() {
        return !isFinishing() && !isDestroyed();
    }
}
