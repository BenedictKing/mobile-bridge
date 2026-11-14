package com.mobilebridge.app;

import android.Manifest;
import android.app.Activity;
import android.app.DownloadManager;
import android.content.ClipData;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.net.ConnectivityManager;
import android.net.Network;
import android.net.NetworkCapabilities;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.os.Handler;
import android.os.Looper;
import android.content.res.Configuration;
import android.graphics.drawable.GradientDrawable;
import android.text.InputType;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowInsets;
import android.view.inputmethod.EditorInfo;
import android.webkit.JavascriptInterface;
import android.webkit.CookieManager;
import android.webkit.URLUtil;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebStorage;
import android.webkit.WebView;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ArrayAdapter;
import android.widget.AdapterView;
import android.widget.Spinner;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.net.URI;
import java.util.ArrayList;
import java.util.List;
import java.util.UUID;

public final class MainActivity extends Activity {
    private static final String PREFS = "mobile-bridge";
    private static final String PUBLIC_URL = "public-url";
    private static final String GATEWAYS = "gateways";
    private static final String ACTIVE_GATEWAY = "active-gateway";
    private static final int FILE_CHOOSER = 41;
    private static final int LOCAL_NETWORK_PERMISSION = 42;
    private static final int DOWNLOAD_PERMISSION = 43;
    // 与 web/presentation.css 的 --page / --text / --muted / --error / --accent 保持一致。
    private static final int ACCENT = 0xff4664ed;
    private static final int[] LIGHT = {0xffffffff, 0xff20242c, 0xff626975, 0xffb43838, 0xffe3e6ec};
    private static final int[] DARK = {0xff191a1f, 0xffececf1, 0xffa8adba, 0xfff19c9c, 0xff32353e};

    private final Handler main = new Handler(Looper.getMainLooper());
    private android.widget.FrameLayout root;
    private Spinner gatewaySelector;
    private ArrayAdapter<String> gatewayAdapter;
    private EditText nameInput;
    private EditText urlInput;
    private TextView status;
    private TextView panelError;
    private Button cancelButton;
    private LinearLayout settingsPanel;
    private View scrim;
    private WebView webView;
    private DirectTransport direct;
    private GatewayWebViewClient gatewayClient;
    private LocalRecoveryCoordinator recovery;
    private DirectTransport.VerifiedRoute pendingRecovery;
    private DirectTransport.VerifiedRoute recoveredRoute;
    private boolean publicLoaded;
    private boolean publicFailed;
    private boolean localShell;
    private long connectStartedAt;
    private ConnectivityManager.NetworkCallback networkCallback;
    private ValueCallback<Uri[]> fileCallback;
    private Runnable pendingDownload;
    private final List<GatewayProfile> gateways = new ArrayList<>();
    private String selectedGatewayId = "";
    private String activeGatewayId = "";
    private boolean syncingGatewaySelector;
    private int connectionGeneration;
    private boolean dark;
    // 网页在 JS 线程读取当前线路，主线程写入；整体替换一个 JSON 串，避免读到半更新的 mode/host。
    private volatile String route = routeJson("public", "");

    @Override
    public void onCreate(Bundle state) {
        super.onCreate(state);
        dark = (getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK)
            == Configuration.UI_MODE_NIGHT_YES;
        root = new android.widget.FrameLayout(this);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) getWindow().setDecorFitsSystemWindows(false);
        root.setOnApplyWindowInsetsListener((view, insets) -> {
            int top, bottom;
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                android.graphics.Insets bars = insets.getInsets(WindowInsets.Type.systemBars());
                // targetSdk 35+ 强制全面屏后 adjustResize 不再缩小窗口，键盘高度要自己让出来，否则会盖住输入区。
                android.graphics.Insets ime = insets.getInsets(WindowInsets.Type.ime());
                top = bars.top;
                bottom = Math.max(bars.bottom, ime.bottom);
            } else {
                top = insets.getSystemWindowInsetTop();
                bottom = insets.getSystemWindowInsetBottom();
            }
            view.setPadding(0, top, 0, bottom);
            return insets;
        });

        webView = new WebView(this);
        root.addView(webView, new android.widget.FrameLayout.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        // 网关入口在网页自己的标题栏里（网页检测到 MobileBridgeNative 后显示）；
        // 这里不再悬浮任何按钮盖在页面上，只在需要输入地址时给一层遮罩。
        scrim = new View(this);
        scrim.setBackgroundColor(0x66000000);
        scrim.setVisibility(View.GONE);
        scrim.setOnClickListener(view -> { if (canLeaveSettings()) hideSettings(); });
        root.addView(scrim, new android.widget.FrameLayout.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        gateways.addAll(loadGatewayProfiles());
        activeGatewayId = preferences().getString(ACTIVE_GATEWAY, "");
        ensureGatewaySelection();
        settingsPanel = buildSettingsPanel();
        android.widget.FrameLayout.LayoutParams panelParams = new android.widget.FrameLayout.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        panelParams.setMargins(dp(16), dp(12), dp(16), 0);
        root.addView(settingsPanel, panelParams);
        setContentView(root);
        applyTheme();
        configureWebView();
        registerBackHandler();
        requestLocalNetworkPermission();
        watchNetworks();
        GatewayProfile active = findGateway(activeGatewayId);
        if (active == null || active.origin.isEmpty()) showSettings(null);
        else connect();
    }

    private LinearLayout buildSettingsPanel() {
        LinearLayout panel = new LinearLayout(this);
        panel.setOrientation(LinearLayout.VERTICAL);
        panel.setPadding(dp(20), dp(18), dp(20), dp(16));
        panel.setElevation(dp(8));
        panel.setVisibility(View.GONE);

        TextView title = new TextView(this);
        title.setText("电脑网关");
        title.setTextSize(18);
        title.setTypeface(android.graphics.Typeface.DEFAULT_BOLD);
        title.setTag("ink");
        panel.addView(title);

        TextView hint = new TextView(this);
        hint.setText("保存多台电脑网关，随时切换。地址只写 HTTPS 域名，不带路径；同一局域网时会自动走局域网加速。");
        hint.setTextSize(13);
        hint.setLineSpacing(0, 1.3f);
        hint.setPadding(0, dp(6), 0, dp(12));
        hint.setTag("muted");
        panel.addView(hint);

        status = new TextView(this);
        status.setTextSize(13);
        status.setPadding(0, 0, 0, dp(8));
        status.setTag("muted");
        status.setVisibility(View.GONE);
        panel.addView(status);

        TextView gatewayLabel = new TextView(this);
        gatewayLabel.setText("已保存的网关");
        gatewayLabel.setTextSize(13);
        gatewayLabel.setTag("muted");
        panel.addView(gatewayLabel);

        gatewayAdapter = new ArrayAdapter<String>(this, android.R.layout.simple_spinner_item, new ArrayList<>());
        gatewayAdapter.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item);
        gatewaySelector = new Spinner(this);
        gatewaySelector.setAdapter(gatewayAdapter);
        gatewaySelector.setOnItemSelectedListener(new AdapterView.OnItemSelectedListener() {
            @Override
            public void onItemSelected(AdapterView<?> parent, View view, int position, long id) {
                if (syncingGatewaySelector || position < 0 || position >= gateways.size()) return;
                selectedGatewayId = gateways.get(position).id;
                fillGatewayEditor(gateways.get(position));
                panelError.setVisibility(View.GONE);
            }

            @Override
            public void onNothingSelected(AdapterView<?> parent) {
                // Keep the current editor values when the spinner has no rows.
            }
        });
        panel.addView(gatewaySelector, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(52)));

        nameInput = new EditText(this);
        nameInput.setSingleLine(true);
        nameInput.setHint("网关名称，例如家里 Mac");
        nameInput.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_CAP_SENTENCES);
        nameInput.setTag("ink");
        panel.addView(nameInput, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(52)));

        urlInput = new EditText(this);
        urlInput.setSingleLine(true);
        urlInput.setHint("https://codex.example.com");
        urlInput.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        urlInput.setImeOptions(EditorInfo.IME_ACTION_GO);
        urlInput.setOnEditorActionListener((view, action, event) -> {
            if (action != EditorInfo.IME_ACTION_GO) return false;
            connect();
            return true;
        });
        urlInput.setText(preferences().getString(PUBLIC_URL, ""));
        urlInput.setTag("ink");
        panel.addView(urlInput, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(52)));

        LinearLayout profileActions = new LinearLayout(this);
        profileActions.setOrientation(LinearLayout.HORIZONTAL);
        profileActions.setGravity(Gravity.END);
        Button newGateway = new Button(this, null, android.R.attr.borderlessButtonStyle);
        newGateway.setText("新建");
        newGateway.setOnClickListener(view -> newGateway());
        profileActions.addView(newGateway, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, dp(48)));
        Button deleteGateway = new Button(this, null, android.R.attr.borderlessButtonStyle);
        deleteGateway.setText("删除");
        deleteGateway.setOnClickListener(view -> deleteGateway());
        profileActions.addView(deleteGateway, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, dp(48)));
        panel.addView(profileActions);

        panelError = new TextView(this);
        panelError.setTextSize(13);
        panelError.setPadding(0, dp(6), 0, 0);
        panelError.setVisibility(View.GONE);
        panel.addView(panelError);

        LinearLayout actions = new LinearLayout(this);
        actions.setOrientation(LinearLayout.HORIZONTAL);
        actions.setGravity(Gravity.END);
        actions.setPadding(0, dp(12), 0, 0);
        cancelButton = new Button(this, null, android.R.attr.borderlessButtonStyle);
        cancelButton.setText("取消");
        cancelButton.setOnClickListener(view -> hideSettings());
        actions.addView(cancelButton, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, dp(48)));
        Button connect = new Button(this);
        connect.setText("连接");
        connect.setTextColor(0xffffffff);
        connect.setBackgroundTintList(android.content.res.ColorStateList.valueOf(ACCENT));
        connect.setOnClickListener(view -> connect());
        LinearLayout.LayoutParams connectParams = new LinearLayout.LayoutParams(dp(104), dp(48));
        connectParams.setMarginStart(dp(8));
        actions.addView(connect, connectParams);
        panel.addView(actions);
        refreshGatewayEditor();
        return panel;
    }

    private SharedPreferences preferences() {
        return getSharedPreferences(PREFS, MODE_PRIVATE);
    }

    private List<GatewayProfile> loadGatewayProfiles() {
        List<GatewayProfile> result = new ArrayList<>();
        String encoded = preferences().getString(GATEWAYS, "");
        if (!encoded.isEmpty()) {
            try {
                JSONArray values = new JSONArray(encoded);
                for (int i = 0; i < values.length(); i++) {
                    GatewayProfile profile = GatewayProfile.fromJson(values.optJSONObject(i));
                    if (profile == null) continue;
                    if (!profile.origin.isEmpty()) {
                        try {
                            profile.origin = normalizeOrigin(profile.origin).toString();
                        } catch (Exception invalid) {
                            continue;
                        }
                    }
                    result.add(profile);
                }
            } catch (Exception ignored) {
                // A corrupt list must not prevent the user from adding a new gateway.
            }
        }
        if (result.isEmpty()) {
            String legacy = preferences().getString(PUBLIC_URL, "").trim();
            if (!legacy.isEmpty()) {
                try {
                    result.add(new GatewayProfile("legacy", "默认网关", normalizeOrigin(legacy).toString()));
                } catch (Exception ignored) {
                    // Ignore the old value if it no longer meets the HTTPS-origin contract.
                }
            }
        }
        return result;
    }

    private void saveGatewayProfiles() {
        JSONArray values = new JSONArray();
        for (GatewayProfile profile : gateways) values.put(profile.toJson());
        GatewayProfile active = findGateway(activeGatewayId);
        preferences().edit()
            .putString(GATEWAYS, values.toString())
            .putString(ACTIVE_GATEWAY, activeGatewayId == null ? "" : activeGatewayId)
            .putString(PUBLIC_URL, active == null ? "" : active.origin)
            .apply();
    }

    private GatewayProfile findGateway(String id) {
        if (id == null || id.isEmpty()) return null;
        for (GatewayProfile profile : gateways) if (id.equals(profile.id)) return profile;
        return null;
    }

    private void ensureGatewaySelection() {
        if (findGateway(activeGatewayId) != null) {
            selectedGatewayId = activeGatewayId;
            return;
        }
        if (gateways.isEmpty()) {
            selectedGatewayId = "";
            activeGatewayId = "";
            return;
        }
        selectedGatewayId = gateways.get(0).id;
        activeGatewayId = selectedGatewayId;
    }

    private void refreshGatewayEditor() {
        if (gatewaySelector == null) return;
        syncingGatewaySelector = true;
        gatewayAdapter.clear();
        for (GatewayProfile profile : gateways) gatewayAdapter.add(profile.displayName());
        gatewayAdapter.notifyDataSetChanged();
        GatewayProfile selected = findGateway(selectedGatewayId);
        int position = selected == null ? -1 : gateways.indexOf(selected);
        if (position >= 0) {
            gatewaySelector.setSelection(position);
            fillGatewayEditor(selected);
        } else {
            nameInput.setText("");
            urlInput.setText("");
        }
        syncingGatewaySelector = false;
    }

    private void fillGatewayEditor(GatewayProfile profile) {
        nameInput.setText(profile.name == null ? "" : profile.name);
        urlInput.setText(profile.origin == null ? "" : profile.origin);
    }

    private void newGateway() {
        GatewayProfile profile = new GatewayProfile(UUID.randomUUID().toString(), "新网关", "");
        gateways.add(profile);
        selectedGatewayId = profile.id;
        refreshGatewayEditor();
        saveGatewayProfiles();
        nameInput.requestFocus();
    }

    private void deleteGateway() {
        GatewayProfile selected = findGateway(selectedGatewayId);
        if (selected == null) return;
        boolean wasActive = selected.id.equals(activeGatewayId);
        clearGatewaySession(selected);
        gateways.remove(selected);
        selectedGatewayId = gateways.isEmpty() ? "" : gateways.get(0).id;
        if (wasActive) {
            connectionGeneration++;
            activeGatewayId = "";
            if (direct != null) {
                direct.close();
                direct = null;
            }
            webView.stopLoading();
            webView.setVisibility(View.INVISIBLE);
            route = routeJson("public", "");
        }
        saveGatewayProfiles();
        refreshGatewayEditor();
        showSettings(null);
    }

    private void clearGatewaySession(GatewayProfile profile) {
        if (profile.origin == null || profile.origin.isEmpty()) return;
        String cookie = CookieManager.getInstance().getCookie(profile.origin + "/");
        if (cookie != null) {
            for (String pair : cookie.split(";")) {
                int separator = pair.indexOf('=');
                if (separator <= 0) continue;
                String name = pair.substring(0, separator).trim();
                if (!name.isEmpty()) {
                    CookieManager.getInstance().setCookie(profile.origin + "/",
                        name + "=; Max-Age=0; Path=/");
                }
            }
            CookieManager.getInstance().flush();
        }
        WebStorage.getInstance().deleteOrigin(profile.origin);
    }

    private void configureWebView() {
        WebSettings settings = webView.getSettings();
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(true);
        settings.setAllowFileAccess(false);
        settings.setMixedContentMode(WebSettings.MIXED_CONTENT_NEVER_ALLOW);
        CookieManager.getInstance().setAcceptCookie(true);
        CookieManager.getInstance().setAcceptThirdPartyCookies(webView, false);
        WebView.setWebContentsDebuggingEnabled((getApplicationInfo().flags & android.content.pm.ApplicationInfo.FLAG_DEBUGGABLE) != 0);
        // 只暴露两个纯界面动作，不读写任何数据；网页据此在标题栏显示网关入口并同步系统栏深浅色。
        webView.addJavascriptInterface(new NativeBridge(), "MobileBridgeNative");
        webView.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(WebView view, ValueCallback<Uri[]> callback,
                                             FileChooserParams params) {
                if (fileCallback != null) fileCallback.onReceiveValue(null);
                fileCallback = callback;
                Intent intent = new Intent(Intent.ACTION_OPEN_DOCUMENT);
                intent.addCategory(Intent.CATEGORY_OPENABLE);
                intent.setType("*/*");
                intent.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true);
                startActivityForResult(intent, FILE_CHOOSER);
                return true;
            }
        });
        webView.setDownloadListener((url, userAgent, contentDisposition, mimeType, contentLength) ->
            startDownload(url, userAgent, contentDisposition, mimeType));
    }

    private void startDownload(String url, String userAgent, String contentDisposition, String mimeType) {
        GatewayProfile active = findGateway(activeGatewayId);
        URI target;
        try {
            if (active == null) throw new IllegalArgumentException("No active gateway");
            URI publicOrigin = URI.create(active.origin);
            target = localShell && recoveredRoute != null
                ? DownloadPolicy.resolve(publicOrigin, recoveredRoute.candidate.origin, url)
                : DownloadPolicy.resolve(publicOrigin, url);
            if (target == null) throw new IllegalArgumentException("Download is not a gateway artifact");
        } catch (Exception invalid) {
            Toast.makeText(this, "无法下载：附件地址无效", Toast.LENGTH_LONG).show();
            return;
        }
        String name = URLUtil.guessFileName(url, contentDisposition, mimeType);
        DirectTransport.DownloadTarget local = direct == null ? null : direct.downloadTarget(target);
        if (localShell && recoveredRoute != null) {
            String path = target.getRawPath();
            if (target.getRawQuery() != null) path += "?" + target.getRawQuery();
            local = new DirectTransport.DownloadTarget(recoveredRoute.candidate.origin.resolve(path), recoveredRoute.routeCookie);
        }
        URI destination = local == null ? target : local.uri;
        DownloadManager.Request request = new DownloadManager.Request(Uri.parse(destination.toString()))
            .setTitle(name)
            .setDescription("来自 " + active.displayName())
            .setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
            .setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, name);
        if (mimeType != null && !mimeType.isEmpty()) request.setMimeType(mimeType);
        if (userAgent != null && !userAgent.isEmpty()) request.addRequestHeader("User-Agent", userAgent);
        // 系统下载器独立于 WebView，必须显式携带当前网关或已验证直连路由的登录 Cookie。
        String cookie = local == null ? CookieManager.getInstance().getCookie(active.origin + "/") : local.cookie;
        if (cookie != null && !cookie.isEmpty()) request.addRequestHeader("Cookie", cookie);
        if (local != null) {
            request.addRequestHeader("Origin", active.origin);
            request.addRequestHeader("X-Mobile-Bridge-Native", "1");
        }
        RouteCredentials.Root rootCookie = local == null ? null : RouteCredentials.root(
            CookieManager.getInstance().getCookie(active.origin + "/"), local.cookie.split("=", 2)[0]);
        JSONObject fallback = rootCookie == null ? null : DownloadFallback.record(active.origin, target, name, mimeType,
            userAgent, rootCookie.cookie());
        Runnable enqueue = () -> enqueueDownload(request, fallback);
        if (Build.VERSION.SDK_INT <= Build.VERSION_CODES.P &&
            checkSelfPermission(Manifest.permission.WRITE_EXTERNAL_STORAGE) != PackageManager.PERMISSION_GRANTED) {
            pendingDownload = enqueue;
            requestPermissions(new String[]{Manifest.permission.WRITE_EXTERNAL_STORAGE}, DOWNLOAD_PERMISSION);
            return;
        }
        enqueue.run();
    }

    private void enqueueDownload(DownloadManager.Request request, JSONObject fallback) {
        try {
            DownloadFallback.enqueue(this, request, fallback);
            Toast.makeText(this, "已开始下载，完成后可在系统下载记录中打开", Toast.LENGTH_LONG).show();
        } catch (Exception error) {
            Toast.makeText(this, "下载失败，请检查系统下载服务和存储空间后重试", Toast.LENGTH_LONG).show();
        }
    }

    private final class NativeBridge {
        @JavascriptInterface
        public void openSettings() {
            main.post(() -> showSettings(null));
        }

        // 页面始终加载公网地址，局域网直连只发生在请求拦截层，网页自己看不出来，需由这里告知。
        @JavascriptInterface
        public String connection() {
            return route;
        }

        @JavascriptInterface
        public void setTheme(boolean value) {
            main.post(() -> {
                if (dark == value) return;
                dark = value;
                applyTheme();
            });
        }
    }

    private void applyTheme() {
        int[] palette = dark ? DARK : LIGHT;
        root.setBackgroundColor(palette[0]);
        webView.setBackgroundColor(palette[0]);
        getWindow().setStatusBarColor(palette[0]);
        getWindow().setNavigationBarColor(palette[0]);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            int light = android.view.WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                | android.view.WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
            getWindow().getInsetsController().setSystemBarsAppearance(dark ? 0 : light, light);
        } else {
            int flags = View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR;
            View decor = getWindow().getDecorView();
            decor.setSystemUiVisibility(dark ? decor.getSystemUiVisibility() & ~flags : decor.getSystemUiVisibility() | flags);
        }
        settingsPanel.setBackground(rounded(palette[0], palette[4], 16));
        panelError.setTextColor(palette[3]);
        cancelButton.setTextColor(palette[2]);
        for (int i = 0; i < settingsPanel.getChildCount(); i++) {
            View child = settingsPanel.getChildAt(i);
            if (!(child instanceof TextView)) continue;
            if ("ink".equals(child.getTag())) ((TextView) child).setTextColor(palette[1]);
            if ("muted".equals(child.getTag())) ((TextView) child).setTextColor(palette[2]);
        }
        urlInput.setHintTextColor(palette[2]);
        nameInput.setHintTextColor(palette[2]);
    }

    private GradientDrawable rounded(int fill, int stroke, int radius) {
        GradientDrawable shape = new GradientDrawable();
        shape.setColor(fill);
        shape.setStroke(dp(1), stroke);
        shape.setCornerRadius(dp(radius));
        return shape;
    }

    private void showSettings(String error) {
        panelError.setText(error == null ? "" : error);
        panelError.setVisibility(error == null ? View.GONE : View.VISIBLE);
        // 没有可返回的页面时不给“取消”：面板是唯一的入口，关掉就只剩空白。
        cancelButton.setVisibility(canLeaveSettings() ? View.VISIBLE : View.GONE);
        settingsPanel.setVisibility(View.VISIBLE);
        scrim.setVisibility(View.VISIBLE);
        urlInput.requestFocus();
        urlInput.setSelection(urlInput.getText().length());
    }

    private boolean canLeaveSettings() {
        return webView.getVisibility() == View.VISIBLE && !activeGatewayId.isEmpty();
    }

    private void hideSettings() {
        settingsPanel.setVisibility(View.GONE);
        scrim.setVisibility(View.GONE);
        ((android.view.inputmethod.InputMethodManager) getSystemService(INPUT_METHOD_SERVICE))
            .hideSoftInputFromWindow(urlInput.getWindowToken(), 0);
    }

    private void showRoute(String mode, String host) {
        route = routeJson(mode, host);
        if ("lan".equals(mode)) status.setText("局域网直连 · " + host);
        else if ("probing".equals(mode)) status.setText("正在探测局域网…");
        else status.setText("公网连接");
        status.setVisibility(View.VISIBLE);
        webView.evaluateJavascript("window.dispatchEvent(new CustomEvent('mobilebridge-route',{detail:"
            + route + "}))", null);
    }

    private static String routeJson(String mode, String host) {
        try {
            return new JSONObject().put("mode", mode).put("host", host).toString();
        } catch (Exception impossible) {
            return "{\"mode\":\"public\",\"host\":\"\"}";
        }
    }

    private void connect() {
        URI origin;
        try {
            origin = normalizeOrigin(urlInput.getText().toString().trim());
        } catch (Exception error) {
            showSettings("地址格式不对：需要 https:// 开头，且只写域名，例如 https://codex.example.com");
            return;
        }
        GatewayProfile profile = findGateway(selectedGatewayId);
        if (profile == null) {
            profile = new GatewayProfile(UUID.randomUUID().toString(), "新网关", "");
            gateways.add(profile);
            selectedGatewayId = profile.id;
        }
        String name = nameInput.getText().toString().trim();
        profile.name = name.isEmpty() ? origin.getHost() : name;
        profile.origin = origin.toString();
        activeGatewayId = profile.id;
        saveGatewayProfiles();
        refreshGatewayEditor();
        hideSettings();
        int epoch = ++connectionGeneration;
        if (direct != null) direct.close();
        if (gatewayClient != null) gatewayClient.close();
        if (recovery != null) recovery.close();
        recovery = null;
        pendingRecovery = null;
        publicLoaded = false;
        publicFailed = false;
        localShell = false;
        recoveredRoute = null;
        connectStartedAt = System.currentTimeMillis();
        route = routeJson("public", "");
        GatewayProfile connectedProfile = profile;
        direct = new DirectTransport(origin, new DirectTransport.Listener() {
            @Override
            public void onRoute(String mode, String host) {
                main.post(() -> { if (epoch == connectionGeneration && !localShell) showRoute(mode, host); });
            }

            @Override
            public void onVerified(DirectTransport.VerifiedRoute verifiedRoute) {
                main.post(() -> {
                    if (epoch != connectionGeneration) return;
                    connectedProfile.recordAuthentication(verifiedRoute.auth, verifiedRoute.candidate.origin.toString());
                    saveGatewayProfiles();
                });
            }
        });
        webView.stopLoading();
        webView.setVisibility(View.VISIBLE);
        gatewayClient = new GatewayWebViewClient(this, origin, direct, new GatewayWebViewClient.PageListener() {
            @Override
            public void onLoaded() {
                if (epoch != connectionGeneration) return;
                publicLoaded = true;
                publicFailed = false;
                pendingRecovery = null;
                if (recovery != null) recovery.close();
                recovery = null;
            }

            @Override
            public void onFailed(String reason) {
                if (epoch != connectionGeneration) return;
                if (!localShell) {
                    publicFailed = true;
                    if (pendingRecovery != null) {
                        loadLocalRecovery(pendingRecovery, epoch);
                        return;
                    }
                    if (recovery != null) {
                        showRoute("probing", "");
                        return;
                    }
                }
                failConnection(connectedProfile, reason);
            }

            @Override
            public void onAuthenticated(JSONObject auth) {
                main.post(() -> {
                    if (epoch != connectionGeneration) return;
                    connectedProfile.recordAuthentication(auth, localShell ? webView.getUrl() : null);
                    saveGatewayProfiles();
                });
            }
        });
        webView.setWebViewClient(gatewayClient);
        webView.loadUrl(origin.toString());
        startLocalRecovery(connectedProfile, origin, epoch);
    }

    private void startLocalRecovery(GatewayProfile profile, URI publicOrigin, int epoch) {
        String cookie = CookieManager.getInstance().getCookie(publicOrigin.toString());
        if (cookie == null || profile.deviceId.isEmpty()) return;
        RouteCredentials.Root rootCookie = RouteCredentials.root(cookie, profile.cookieName);
        if (rootCookie == null) return;
        recovery = new LocalRecoveryCoordinator(this, profile, publicOrigin, rootCookie.cookie(),
            new LocalRecoveryCoordinator.Listener() {
                @Override
                public void onRecovered(DirectTransport.VerifiedRoute route) {
                    if (epoch != connectionGeneration || publicLoaded || localShell) return;
                    profile.recordAuthentication(route.auth, route.candidate.origin.toString());
                    saveGatewayProfiles();
                    pendingRecovery = route;
                    long elapsed = System.currentTimeMillis() - connectStartedAt;
                    if (publicFailed || elapsed >= 2000) loadLocalRecovery(route, epoch);
                    else main.postDelayed(() -> loadLocalRecovery(route, epoch), 2000 - elapsed);
                }

                @Override
                public void onTimeout() {
                    if (epoch != connectionGeneration || publicLoaded || localShell) return;
                    recovery = null;
                    failConnection(profile, "未找到同一局域网中的网关");
                }
            });
        recovery.start();
    }

    private void loadLocalRecovery(DirectTransport.VerifiedRoute route, int epoch) {
        if (epoch != connectionGeneration || publicLoaded || localShell || pendingRecovery != route) return;
        localShell = true;
        pendingRecovery = null;
        if (recovery != null) recovery.close();
        recovery = null;
        URI local = route.candidate.origin;
        int equals = route.routeCookie.indexOf('=');
        if (equals <= 0) {
            failConnection(findGateway(activeGatewayId), "已发现网关，但无法安全安装路由凭据");
            return;
        }
        String cookie = route.routeCookie + "; Path=/; HttpOnly; SameSite=Lax";
        CookieManager.getInstance().setCookie(local.toString(), cookie, accepted -> {
            if (epoch != connectionGeneration) return;
            if (!Boolean.TRUE.equals(accepted)) {
                failConnection(findGateway(activeGatewayId), "已发现网关，但无法安全启动局域网页面");
                return;
            }
            CookieManager.getInstance().flush();
            recoveredRoute = route;
            direct.clear();
            webView.stopLoading();
            webView.setVisibility(View.VISIBLE);
            showRoute("lan", local.getHost());
            webView.loadUrl(local.toString());
        });
    }

    private void failConnection(GatewayProfile profile, String reason) {
        webView.setVisibility(View.INVISIBLE);
        String name = profile == null ? "网关" : profile.name;
        showSettings("无法打开 " + name + "（" + reason + "）。请确认电脑网关在运行、地址正确，且手机与电脑允许局域网互通。");
    }

    static URI normalizeOrigin(String value) throws Exception {
        URI uri = URI.create(value);
        if (!"https".equalsIgnoreCase(uri.getScheme()) || uri.getHost() == null || uri.getUserInfo() != null
            || uri.getRawQuery() != null || uri.getRawFragment() != null
            || (uri.getRawPath() != null && !uri.getRawPath().isEmpty() && !"/".equals(uri.getRawPath()))) {
            throw new IllegalArgumentException("HTTPS origin required");
        }
        return new URI("https", null, uri.getHost(), uri.getPort(), null, null, null);
    }

    private void requestLocalNetworkPermission() {
        if (Build.VERSION.SDK_INT >= 37) {
            requestPermissions(new String[]{"android.permission.ACCESS_LOCAL_NETWORK"}, LOCAL_NETWORK_PERMISSION);
        }
    }

    private void watchNetworks() {
        ConnectivityManager manager = (ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE);
        networkCallback = new ConnectivityManager.NetworkCallback() {
            @Override
            public void onCapabilitiesChanged(Network network, NetworkCapabilities capabilities) {
                if (direct != null) direct.networkChanged();
            }

            @Override
            public void onLost(Network network) {
                if (direct != null) direct.networkChanged();
            }
        };
        manager.registerDefaultNetworkCallback(networkCallback);
    }

    private void registerBackHandler() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            getOnBackInvokedDispatcher().registerOnBackInvokedCallback(
                android.window.OnBackInvokedDispatcher.PRIORITY_DEFAULT,
                () -> {
                    if (settingsPanel != null && settingsPanel.getVisibility() == View.VISIBLE) {
                        // 面板背后没有页面时，返回键直接退出，不留下空白屏。
                        if (canLeaveSettings()) hideSettings();
                        else finish();
                        return;
                    }
                    if (webView != null && webView.canGoBack()) {
                        webView.goBack();
                    } else {
                        finish();
                    }
                }
            );
        }
    }

    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode != FILE_CHOOSER || fileCallback == null) return;
        List<Uri> files = new ArrayList<>();
        if (resultCode == RESULT_OK && data != null) {
            if (data.getData() != null) files.add(data.getData());
            ClipData clip = data.getClipData();
            if (clip != null) {
                for (int i = 0; i < clip.getItemCount(); i++) {
                    files.add(clip.getItemAt(i).getUri());
                }
            }
        }
        fileCallback.onReceiveValue(files.toArray(new Uri[0]));
        fileCallback = null;
    }

    @Override
    public void onRequestPermissionsResult(int requestCode, String[] permissions, int[] grantResults) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults);
        if (requestCode != DOWNLOAD_PERMISSION) return;
        Runnable request = pendingDownload;
        pendingDownload = null;
        if (request == null) return;
        if (grantResults.length > 0 && grantResults[0] == PackageManager.PERMISSION_GRANTED) {
            request.run();
        } else {
            Toast.makeText(this, "需要存储权限才能保存下载文件", Toast.LENGTH_LONG).show();
        }
    }

    @Override
    @SuppressWarnings("deprecation")
    // Android 13+ 手势已由 registerBackHandler 的原生回调处理；此处保留旧系统返回键兼容。
    @android.annotation.SuppressLint("GestureBackNavigation")
    public void onBackPressed() {
        if (settingsPanel != null && settingsPanel.getVisibility() == View.VISIBLE) {
            if (canLeaveSettings()) hideSettings();
            else super.onBackPressed();
            return;
        }
        if (webView != null && webView.canGoBack()) {
            webView.goBack();
        } else {
            super.onBackPressed();
        }
    }

    @Override
    protected void onStart() {
        super.onStart();
        if (gatewayClient != null) gatewayClient.resumeDiscovery();
        if (webView != null && !publicLoaded && !localShell && recovery == null) {
            GatewayProfile profile = findGateway(activeGatewayId);
            if (profile != null) {
                try { startLocalRecovery(profile, URI.create(profile.origin), connectionGeneration); }
                catch (Exception ignored) {}
            }
        }
    }

    @Override
    protected void onStop() {
        if (gatewayClient != null) gatewayClient.pauseDiscovery();
        if (recovery != null) recovery.close();
        recovery = null;
        pendingRecovery = null;
        super.onStop();
    }

    @Override
    protected void onDestroy() {
        if (networkCallback != null) {
            ((ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE)).unregisterNetworkCallback(networkCallback);
        }
        if (direct != null) direct.close();
        if (gatewayClient != null) gatewayClient.close();
        if (recovery != null) recovery.close();
        if (fileCallback != null) fileCallback.onReceiveValue(null);
        webView.destroy();
        super.onDestroy();
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }
}
