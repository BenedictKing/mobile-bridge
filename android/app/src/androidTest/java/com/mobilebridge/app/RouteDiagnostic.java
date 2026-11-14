package com.mobilebridge.app;

import android.app.Instrumentation;
import android.os.Bundle;
import android.webkit.CookieManager;
import java.net.URI;
import java.util.HashMap;
import java.util.Map;
import org.json.JSONArray;
import org.json.JSONObject;

/** 只输出路由诊断状态，禁止输出原始登录凭据。 */
public final class RouteDiagnostic extends Instrumentation {
    private Bundle args;
    @Override public void onCreate(Bundle args) { super.onCreate(args); this.args = args; start(); }
    @Override public void onStart() {
        Bundle result = new Bundle();
        try {
            String origin = args.getString("origin");
            String local = args.getString("local");
            String cookies = CookieManager.getInstance().getCookie(origin);
            JSONArray names = new JSONArray();
            for (String part : cookies.split(";")) names.put(part.split("=", 2)[0].trim());
            JSONObject auth = auth(local, cookies, origin);
            String expected = auth.optString("cookieName");
            RouteCredentials.Root chosen = RouteCredentials.root(cookies, expected);
            JSONObject report = new JSONObject().put("cookieNames", names)
                .put("expected", expected).put("chosen", chosen == null ? "" : chosen.name)
                .put("rootAuthenticated", auth.optBoolean("authenticated"));
            if (chosen != null) report.put("chosenRouteAuthenticated", auth(local,
                chosen.name + "=" + RouteCredentials.routeToken(chosen.token, local), origin).optBoolean("authenticated"));
            result.putString("result", report.toString());
            finish(-1, result);
        } catch (Throwable error) { result.putString("result", error.toString()); finish(0, result); }
    }
    private JSONObject auth(String local, String cookie, String origin) throws Exception {
        Map<String, String> headers = new HashMap<>();
        headers.put("Cookie", cookie); headers.put("Origin", origin); headers.put("X-Mobile-Bridge-Native", "1");
        return new JSONObject(NativeHttp.readStringAndClose(NativeHttp.open(
            URI.create(local + "/api/auth"), "GET", headers, 2000, 3000)));
    }
}
