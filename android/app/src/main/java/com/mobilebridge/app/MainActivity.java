package com.mobilebridge.app;

import android.app.Activity;
import android.content.ClipData;
import android.content.Intent;
import android.content.SharedPreferences;
import android.net.ConnectivityManager;
import android.net.Network;
import android.net.NetworkCapabilities;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
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
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.TextView;

import java.net.URI;
import java.util.ArrayList;
import java.util.List;

public final class MainActivity extends Activity {
    private static final String PREFS = "mobile-bridge";
    private static final String PUBLIC_URL = "public-url";
    private static final int FILE_CHOOSER = 41;
    private static final int LOCAL_NETWORK_PERMISSION = 42;
    // 与 web/presentation.css 的 --page / --text / --muted / --error / --accent 保持一致。
    private static final int ACCENT = 0xff4664ed;
    private static final int[] LIGHT = {0xffffffff, 0xff20242c, 0xff626975, 0xffb43838, 0xffe3e6ec};
    private static final int[] DARK = {0xff191a1f, 0xffececf1, 0xffa8adba, 0xfff19c9c, 0xff32353e};

    private final Handler main = new Handler(Looper.getMainLooper());
    private android.widget.FrameLayout root;
    private EditText urlInput;
    private TextView status;
    private final Runnable hideStatus = () -> status.setVisibility(View.GONE);
    private TextView panelError;
    private Button settingsButton;
    private Button cancelButton;
    private LinearLayout settingsPanel;
    private WebView webView;
    private DirectTransport direct;
    private ConnectivityManager.NetworkCallback networkCallback;
    private ValueCallback<Uri[]> fileCallback;
    // 网页正常加载后，原生按钮让位给网页自己的页头；网页设置里的“修改网关地址”再把面板叫出来。
    private boolean pageLoaded;
    private boolean dark;

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

        status = new TextView(this);
        status.setPadding(dp(14), dp(7), dp(14), dp(7));
        status.setTextSize(13);
        status.setElevation(dp(3));
        status.setVisibility(View.GONE);
        android.widget.FrameLayout.LayoutParams statusParams = new android.widget.FrameLayout.LayoutParams(
            ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        statusParams.gravity = Gravity.TOP | Gravity.CENTER_HORIZONTAL;
        statusParams.setMargins(0, dp(60), 0, 0);
        root.addView(status, statusParams);

        settingsButton = new Button(this);
        settingsButton.setText("⚙");
        settingsButton.setTextSize(20);
        settingsButton.setPadding(0, 0, 0, 0);
        settingsButton.setContentDescription("连接设置");
        settingsButton.setOnClickListener(view -> toggleSettings());
        android.widget.FrameLayout.LayoutParams settingsParams = new android.widget.FrameLayout.LayoutParams(dp(48), dp(48));
        settingsParams.gravity = Gravity.TOP | Gravity.END;
        settingsParams.setMargins(0, dp(8), dp(12), 0);
        root.addView(settingsButton, settingsParams);

        settingsPanel = buildSettingsPanel();
        android.widget.FrameLayout.LayoutParams panelParams = new android.widget.FrameLayout.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        panelParams.setMargins(dp(16), dp(64), dp(16), 0);
        root.addView(settingsPanel, panelParams);
        setContentView(root);
        applyTheme();
        configureWebView();
        registerBackHandler();
        requestLocalNetworkPermission();
        watchNetworks();
        String saved = preferences().getString(PUBLIC_URL, "");
        if (saved.isEmpty()) showSettings(null);
        else connect();
    }

    private LinearLayout buildSettingsPanel() {
        LinearLayout panel = new LinearLayout(this);
        panel.setOrientation(LinearLayout.VERTICAL);
        panel.setPadding(dp(20), dp(18), dp(20), dp(16));
        panel.setElevation(dp(8));
        panel.setVisibility(View.GONE);

        TextView title = new TextView(this);
        title.setText("连接电脑网关");
        title.setTextSize(18);
        title.setTypeface(android.graphics.Typeface.DEFAULT_BOLD);
        title.setTag("ink");
        panel.addView(title);

        TextView hint = new TextView(this);
        hint.setText("填写网关的 HTTPS 地址（只写域名，不带路径）。同一局域网时会自动走局域网加速。");
        hint.setTextSize(13);
        hint.setLineSpacing(0, 1.3f);
        hint.setPadding(0, dp(6), 0, dp(12));
        hint.setTag("muted");
        panel.addView(hint);

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
        return panel;
    }

    private SharedPreferences preferences() {
        return getSharedPreferences(PREFS, MODE_PRIVATE);
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
        // 只暴露两个纯界面动作，不读写任何数据；网页据此在设置里显示“修改网关地址”并同步系统栏深浅色。
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
    }

    private final class NativeBridge {
        @JavascriptInterface
        public void openSettings() {
            main.post(() -> showSettings(null));
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
        settingsButton.setBackground(rounded(palette[0], palette[4], 24));
        settingsButton.setTextColor(palette[2]);
        status.setBackground(rounded(palette[0], palette[4], 16));
        status.setTextColor(palette[1]);
        panelError.setTextColor(palette[3]);
        cancelButton.setTextColor(palette[2]);
        for (int i = 0; i < settingsPanel.getChildCount(); i++) {
            View child = settingsPanel.getChildAt(i);
            if (!(child instanceof TextView)) continue;
            if ("ink".equals(child.getTag())) ((TextView) child).setTextColor(palette[1]);
            if ("muted".equals(child.getTag())) ((TextView) child).setTextColor(palette[2]);
        }
        urlInput.setHintTextColor(palette[2]);
    }

    private GradientDrawable rounded(int fill, int stroke, int radius) {
        GradientDrawable shape = new GradientDrawable();
        shape.setColor(fill);
        shape.setStroke(dp(1), stroke);
        shape.setCornerRadius(dp(radius));
        return shape;
    }

    private void toggleSettings() {
        if (settingsPanel.getVisibility() == View.VISIBLE) hideSettings();
        else showSettings(null);
    }

    private void showSettings(String error) {
        panelError.setText(error == null ? "" : error);
        panelError.setVisibility(error == null ? View.GONE : View.VISIBLE);
        // 首次使用没有可返回的页面，不给“取消”。
        cancelButton.setVisibility(preferences().getString(PUBLIC_URL, "").isEmpty() ? View.GONE : View.VISIBLE);
        settingsPanel.setVisibility(View.VISIBLE);
        settingsButton.setVisibility(View.GONE);
        urlInput.requestFocus();
        urlInput.setSelection(urlInput.getText().length());
    }

    private void hideSettings() {
        settingsPanel.setVisibility(View.GONE);
        settingsButton.setVisibility(pageLoaded ? View.GONE : View.VISIBLE);
        ((android.view.inputmethod.InputMethodManager) getSystemService(INPUT_METHOD_SERVICE))
            .hideSoftInputFromWindow(urlInput.getWindowToken(), 0);
    }

    private void showStatus(String value) {
        status.setText(value);
        status.setVisibility(View.VISIBLE);
        main.removeCallbacks(hideStatus);
        // 探测中的提示一直显示；有结果后停留片刻就收起，不长期压在网页页头上。
        if (!value.endsWith("…")) main.postDelayed(hideStatus, 2500);
    }

    private void connect() {
        URI origin;
        try {
            origin = normalizeOrigin(urlInput.getText().toString().trim());
        } catch (Exception error) {
            showSettings("地址格式不对：需要 https:// 开头，且只写域名，例如 https://codex.example.com");
            return;
        }
        preferences().edit().putString(PUBLIC_URL, origin.toString()).apply();
        pageLoaded = false;
        hideSettings();
        if (direct != null) direct.close();
        direct = new DirectTransport(origin, value -> main.post(() -> showStatus(value)));
        webView.stopLoading();
        webView.setVisibility(View.VISIBLE);
        webView.setWebViewClient(new GatewayWebViewClient(origin, direct, new GatewayWebViewClient.PageListener() {
            @Override
            public void onLoaded() {
                pageLoaded = true;
                if (settingsPanel.getVisibility() != View.VISIBLE) settingsButton.setVisibility(View.GONE);
            }

            @Override
            public void onFailed(String reason) {
                pageLoaded = false;
                // 不展示 WebView 自带的错误页，直接给出可修改的地址和原因。
                webView.setVisibility(View.INVISIBLE);
                showSettings("无法打开 " + origin.getHost() + "（" + reason + "）。请确认电脑网关在运行、地址正确。");
            }
        }));
        webView.loadUrl(origin.toString());
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
                        hideSettings();
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
    @SuppressWarnings("deprecation")
    public void onBackPressed() {
        if (settingsPanel != null && settingsPanel.getVisibility() == View.VISIBLE) {
            hideSettings();
            return;
        }
        if (webView != null && webView.canGoBack()) {
            webView.goBack();
        } else {
            super.onBackPressed();
        }
    }

    @Override
    protected void onDestroy() {
        if (networkCallback != null) {
            ((ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE)).unregisterNetworkCallback(networkCallback);
        }
        main.removeCallbacks(hideStatus);
        if (direct != null) direct.close();
        if (fileCallback != null) fileCallback.onReceiveValue(null);
        webView.destroy();
        super.onDestroy();
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }
}
