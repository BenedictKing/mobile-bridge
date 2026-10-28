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
import android.view.ViewGroup;
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

    private final Handler main = new Handler(Looper.getMainLooper());
    private EditText urlInput;
    private TextView status;
    private LinearLayout settingsPanel;
    private WebView webView;
    private DirectTransport direct;
    private ConnectivityManager.NetworkCallback networkCallback;
    private ValueCallback<Uri[]> fileCallback;

    @Override
    public void onCreate(Bundle state) {
        super.onCreate(state);
        android.widget.FrameLayout root = new android.widget.FrameLayout(this);
        root.setBackgroundColor(0xfff3f5f8);
        root.setOnApplyWindowInsetsListener((view, insets) -> {
            int top, bottom;
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                android.graphics.Insets systemBars = insets.getInsets(android.view.WindowInsets.Type.systemBars());
                top = systemBars.top;
                bottom = systemBars.bottom;
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

        int padding = dp(12);
        status = new TextView(this);
        status.setPadding(padding, dp(6), padding, dp(6));
        status.setText("公网连接");
        status.setTextSize(13);
        status.setBackground(new android.graphics.drawable.ColorDrawable(0xe0ffffff));
        status.setElevation(dp(2));
        android.widget.FrameLayout.LayoutParams statusParams = new android.widget.FrameLayout.LayoutParams(
            ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        statusParams.setMargins(padding, padding, padding, 0);
        root.addView(status, statusParams);

        Button settingsButton = new Button(this);
        settingsButton.setText("⚙");
        settingsButton.setContentDescription("设置");
        settingsButton.setOnClickListener(view -> toggleSettings());
        android.widget.FrameLayout.LayoutParams settingsParams = new android.widget.FrameLayout.LayoutParams(dp(48), dp(48));
        settingsParams.gravity = android.view.Gravity.TOP | android.view.Gravity.END;
        settingsParams.setMargins(0, padding, padding, 0);
        root.addView(settingsButton, settingsParams);

        settingsPanel = new LinearLayout(this);
        settingsPanel.setOrientation(LinearLayout.VERTICAL);
        settingsPanel.setPadding(padding, padding, padding, padding);
        settingsPanel.setBackground(new android.graphics.drawable.ColorDrawable(0xffffffff));
        settingsPanel.setElevation(dp(8));
        settingsPanel.setVisibility(android.view.View.GONE);
        android.widget.FrameLayout.LayoutParams panelParams = new android.widget.FrameLayout.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        panelParams.setMargins(padding, dp(64), padding, 0);
        root.addView(settingsPanel, panelParams);

        LinearLayout controls = new LinearLayout(this);
        controls.setOrientation(LinearLayout.HORIZONTAL);
        urlInput = new EditText(this);
        urlInput.setSingleLine(true);
        urlInput.setHint("https://codex.example.com");
        urlInput.setText(preferences().getString(PUBLIC_URL, ""));
        controls.addView(urlInput, new LinearLayout.LayoutParams(0, dp(48), 1));
        Button connect = new Button(this);
        connect.setText("连接");
        connect.setOnClickListener(view -> connect());
        controls.addView(connect, new LinearLayout.LayoutParams(dp(88), dp(48)));
        settingsPanel.addView(controls);
        setContentView(root);
        configureWebView();
        registerBackHandler();
        requestLocalNetworkPermission();
        watchNetworks();
        String saved = preferences().getString(PUBLIC_URL, "");
        if (!saved.isEmpty()) connect();
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

    private void toggleSettings() {
        if (settingsPanel.getVisibility() == android.view.View.VISIBLE) {
            settingsPanel.setVisibility(android.view.View.GONE);
        } else {
            settingsPanel.setVisibility(android.view.View.VISIBLE);
            urlInput.requestFocus();
        }
    }

    private void connect() {
        try {
            URI origin = normalizeOrigin(urlInput.getText().toString().trim());
            settingsPanel.setVisibility(android.view.View.GONE);
            preferences().edit().putString(PUBLIC_URL, origin.toString()).apply();
            if (direct != null) direct.close();
            direct = new DirectTransport(origin, value -> main.post(() -> status.setText(value)));
            webView.stopLoading();
            webView.setWebViewClient(new GatewayWebViewClient(origin, direct));
            webView.loadUrl(origin.toString());
        } catch (Exception error) {
            status.setText("请输入不带路径的 HTTPS 地址");
        }
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
                    if (settingsPanel != null && settingsPanel.getVisibility() == android.view.View.VISIBLE) {
                        settingsPanel.setVisibility(android.view.View.GONE);
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
        if (settingsPanel != null && settingsPanel.getVisibility() == android.view.View.VISIBLE) {
            settingsPanel.setVisibility(android.view.View.GONE);
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
        if (direct != null) direct.close();
        if (fileCallback != null) fileCallback.onReceiveValue(null);
        webView.destroy();
        super.onDestroy();
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }
}
