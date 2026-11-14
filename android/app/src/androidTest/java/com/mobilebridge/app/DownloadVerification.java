package com.mobilebridge.app;

import android.app.DownloadManager;
import android.app.Instrumentation;
import android.content.Context;
import android.database.Cursor;
import android.net.Uri;
import android.os.Bundle;
import android.os.Environment;
import android.webkit.CookieManager;
import java.net.URI;
import java.io.InputStream;
import java.security.MessageDigest;

/** 仅测试 APK 提供此入口；用真实文件验证系统下载器与后台回退广播。 */
public final class DownloadVerification extends Instrumentation {
    private Bundle arguments;

    @Override public void onCreate(Bundle arguments) {
        super.onCreate(arguments);
        this.arguments = arguments;
        start();
    }

    @Override public void onStart() {
        Bundle result = new Bundle();
        try {
            Context context = getTargetContext();
            URI url = URI.create(arguments.getString("url"));
            String origin = url.getScheme() + "://" + url.getAuthority();
            String name = "bridge-download-verification-" + System.currentTimeMillis() + ".apk";
            runOnMainSync(() -> CookieManager.getInstance().setCookie(origin, "codex_mobile_session=verification-only"));
            CookieManager.getInstance().flush();
            DownloadManager.Request request = new DownloadManager.Request(Uri.parse(arguments.getString("initial", url.toString())))
                .setTitle(name).setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, name);
            DownloadFallback.enqueue(context, request, DownloadFallback.record(origin, url, name,
                "application/vnd.android.package-archive", "BridgeMobile/verification-Android",
                "codex_mobile_session=verification-only"));
            DownloadManager manager = context.getSystemService(DownloadManager.class);
            long deadline = System.currentTimeMillis() + 180000;
            while (System.currentTimeMillis() < deadline) {
                try (Cursor cursor = manager.query(new DownloadManager.Query())) {
                    while (cursor.moveToNext()) {
                        if (!name.equals(cursor.getString(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_TITLE)))) continue;
                        int status = cursor.getInt(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_STATUS));
                        if (status != DownloadManager.STATUS_SUCCESSFUL) continue;
                        long id = cursor.getLong(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_ID));
                        MessageDigest digest = MessageDigest.getInstance("SHA-256");
                        long bytes = 0;
                        try (InputStream input = context.getContentResolver().openInputStream(manager.getUriForDownloadedFile(id))) {
                            byte[] buffer = new byte[65536];
                            int count;
                            while ((count = input.read(buffer)) != -1) { digest.update(buffer, 0, count); bytes += count; }
                        }
                        String hash = RouteCredentials.hex(digest.digest());
                        if (!hash.equals(arguments.getString("sha256"))) throw new AssertionError("SHA-256 mismatch");
                        result.putString("result", "success bytes=" + bytes + " sha256=" + hash);
                        finish(-1, result);
                        return;
                    }
                }
                Thread.sleep(1000);
            }
            throw new AssertionError("system download timed out");
        } catch (Throwable error) {
            result.putString("result", error.toString());
            finish(0, result);
        }
    }
}
