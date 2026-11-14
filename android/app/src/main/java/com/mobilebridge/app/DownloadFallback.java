package com.mobilebridge.app;

import android.app.DownloadManager;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.database.Cursor;
import android.net.Uri;
import android.os.Environment;
import android.webkit.CookieManager;
import org.json.JSONObject;
import java.net.URI;

/** 系统下载器无法更换任务地址；LAN 最终失败后只回退一次公网。 */
public final class DownloadFallback extends BroadcastReceiver {
    private static final String PREFS = "download-fallback";

    static boolean retryable(int reason) {
        return reason == 401 || reason == 403 || reason == 408 || reason == 429
            || (reason >= 500 && reason <= 599)
            || reason == DownloadManager.ERROR_UNHANDLED_HTTP_CODE
            || reason == DownloadManager.ERROR_HTTP_DATA_ERROR
            || reason == DownloadManager.ERROR_CANNOT_RESUME;
    }

    static JSONObject record(String origin, URI url, String name, String mime, String agent, String cookie) {
        JSONObject value = new JSONObject();
        RouteCredentials.Root root = RouteCredentials.root(cookie);
        try {
            value.put("origin", origin).put("url", url.toString()).put("name", name)
                .put("mime", mime == null ? "" : mime).put("agent", agent == null ? "" : agent)
                .put("session", session(cookie))
                .put("cookieName", root == null ? "" : root.name);
        } catch (Exception error) {
            throw new IllegalArgumentException(error);
        }
        return value;
    }

    static String session(String cookie) {
        RouteCredentials.Root root = RouteCredentials.root(cookie);
        return root == null ? "" : RouteCredentials.identifier(root.token);
    }

    static void enqueue(Context context, DownloadManager.Request request, JSONObject fallback) {
        DownloadManager manager = context.getSystemService(DownloadManager.class);
        if (manager == null) throw new IllegalStateException("Download service unavailable");
        long id = manager.enqueue(request);
        if (fallback != null) {
            context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
                .putString(Long.toString(id), fallback.toString()).commit();
        }
    }

    @Override public void onReceive(Context context, Intent intent) {
        if (!DownloadManager.ACTION_DOWNLOAD_COMPLETE.equals(intent.getAction())) return;
        long id = intent.getLongExtra(DownloadManager.EXTRA_DOWNLOAD_ID, -1);
        SharedPreferences prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
        String key = Long.toString(id);
        String record = prefs.getString(key, "");
        if (record.isEmpty()) return;
        DownloadManager manager = context.getSystemService(DownloadManager.class);
        if (manager == null) return;
        try (Cursor cursor = manager.query(new DownloadManager.Query().setFilterById(id))) {
            if (cursor == null || !cursor.moveToFirst()) {
                prefs.edit().remove(key).commit();
                return;
            }
            int status = cursor.getInt(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_STATUS));
            if (status != DownloadManager.STATUS_FAILED && status != DownloadManager.STATUS_SUCCESSFUL) return;
            // 先消费记录，重复广播不会产生多个重试；公网任务不再写入回退记录。
            prefs.edit().remove(key).commit();
            int reason = cursor.getInt(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_REASON));
            if (status != DownloadManager.STATUS_FAILED || !retryable(reason)) return;
            JSONObject value = new JSONObject(record);
            String origin = value.getString("origin");
            URI url = DownloadPolicy.resolve(URI.create(origin), value.getString("url"));
            if (url == null) return;
            String cookie = CookieManager.getInstance().getCookie(origin + "/");
            RouteCredentials.Root root = RouteCredentials.root(cookie, value.optString("cookieName"));
            cookie = root == null ? null : root.cookie();
            if (cookie == null || session(cookie).isEmpty()
                || !session(cookie).equals(value.optString("session"))) return;
            DownloadManager.Request request = new DownloadManager.Request(Uri.parse(url.toString()))
                .setTitle(value.getString("name"))
                .setDescription("局域网下载失败，已通过公网重新下载")
                .setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
                .setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, value.getString("name"));
            request.addRequestHeader("Cookie", cookie);
            if (!value.optString("agent").isEmpty()) request.addRequestHeader("User-Agent", value.getString("agent"));
            if (!value.optString("mime").isEmpty()) request.setMimeType(value.getString("mime"));
            enqueue(context, request, null);
        } catch (Exception ignored) {
            // 原下载记录保留失败原因，不能让后台广播导致应用崩溃。
        }
    }
}
