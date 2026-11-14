package com.mobilebridge.app;

import android.app.DownloadManager;
import android.app.Instrumentation;
import android.database.Cursor;
import android.os.Bundle;
import java.io.InputStream;
import java.security.MessageDigest;
import org.json.JSONObject;

/** 只读检查聊天点击产生的系统下载记录，确认实际地址、大小和内容。 */
public final class DownloadInspection extends Instrumentation {
    private Bundle args;
    @Override public void onCreate(Bundle args) { super.onCreate(args); this.args = args; start(); }
    @Override public void onStart() {
        Bundle result = new Bundle();
        try {
            long id = Long.parseLong(args.getString("id"));
            DownloadManager manager = getTargetContext().getSystemService(DownloadManager.class);
            try (Cursor cursor = manager.query(new DownloadManager.Query().setFilterById(id))) {
                if (!cursor.moveToFirst()) throw new IllegalArgumentException("Download missing");
                int status = cursor.getInt(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_STATUS));
                JSONObject report = new JSONObject().put("id", id).put("status", status)
                    .put("reason", cursor.getInt(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_REASON)))
                    .put("uri", cursor.getString(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_URI)))
                    .put("description", cursor.getString(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_DESCRIPTION)))
                    .put("bytes", cursor.getLong(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_BYTES_DOWNLOADED_SO_FAR)))
                    .put("total", cursor.getLong(cursor.getColumnIndexOrThrow(DownloadManager.COLUMN_TOTAL_SIZE_BYTES)));
                if (status == DownloadManager.STATUS_SUCCESSFUL) {
                    MessageDigest digest = MessageDigest.getInstance("SHA-256");
                    try (InputStream input = getTargetContext().getContentResolver().openInputStream(manager.getUriForDownloadedFile(id))) {
                        byte[] buffer = new byte[65536]; int count;
                        while ((count = input.read(buffer)) != -1) digest.update(buffer, 0, count);
                    }
                    report.put("sha256", RouteCredentials.hex(digest.digest()));
                }
                result.putString("result", report.toString());
            }
            finish(-1, result);
        } catch (Throwable error) { result.putString("result", error.toString()); finish(0, result); }
    }
}
