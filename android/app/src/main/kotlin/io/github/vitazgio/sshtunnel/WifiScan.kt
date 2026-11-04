package io.github.vitazgio.sshtunnel

import android.Manifest
import android.app.Activity
import android.content.Context
import android.content.pm.PackageManager
import android.location.LocationManager
import android.net.wifi.ScanResult
import android.net.wifi.WifiManager
import android.os.Build
import org.json.JSONArray
import org.json.JSONObject

/**
 * Wi-Fi для «Сканера сети» (вкладки «Сети Wi-Fi» и «Сигнал»): сети вокруг —
 * имя, канал, ширина, уровень, защита — и уровень своей сети прямо сейчас.
 * Android отдаёт это только с разрешением на точную геолокацию и при
 * включённой геолокации; сканировать даёт примерно 4 раза за 2 минуты, так
 * что новый скан просим не чаще раза в 30 с, а между ними отдаём последний.
 */
object WifiScan {
    private var lastScanAt = 0L

    private fun wm(ctx: Context) = ctx.applicationContext.getSystemService(Context.WIFI_SERVICE) as WifiManager

    fun hasPermission(ctx: Context) =
        ctx.checkSelfPermission(Manifest.permission.ACCESS_FINE_LOCATION) == PackageManager.PERMISSION_GRANTED

    fun askPermission(a: Activity) {
        if (!hasPermission(a)) a.requestPermissions(arrayOf(Manifest.permission.ACCESS_FINE_LOCATION), 7301)
    }

    private fun locationOn(ctx: Context): Boolean {
        val lm = ctx.getSystemService(Context.LOCATION_SERVICE) as? LocationManager ?: return true
        return if (Build.VERSION.SDK_INT >= 28) lm.isLocationEnabled else true
    }

    /** Общий заголовок ответа: можно ли вообще, и если нет — почему. */
    private fun head(ctx: Context): JSONObject {
        val o = JSONObject().put("available", true)
        when {
            !hasPermission(ctx) -> o.put("available", false).put("need", "permission")
            !locationOn(ctx) -> o.put("available", false).put("need", "location")
            !wm(ctx).isWifiEnabled -> o.put("wifiOff", true)
        }
        return o
    }

    private fun width(r: ScanResult): Int = when (r.channelWidth) {
        ScanResult.CHANNEL_WIDTH_40MHZ -> 40
        ScanResult.CHANNEL_WIDTH_80MHZ -> 80
        ScanResult.CHANNEL_WIDTH_160MHZ, ScanResult.CHANNEL_WIDTH_80MHZ_PLUS_MHZ -> 160
        5 -> 320 // CHANNEL_WIDTH_320MHZ (Android 13+)
        else -> 20
    }

    @Suppress("DEPRECATION")
    private fun ssidOf(r: ScanResult): String =
        if (Build.VERSION.SDK_INT >= 33) (r.wifiSsid?.toString() ?: "").trim('"') else r.SSID ?: ""

    /** Кнопка «Сканировать»: новый скан. Android даёт ~4 за 2 минуты —
     * чаще раза в 30 с не просим; ответ — через сколько мс можно снова. */
    @Suppress("DEPRECATION")
    fun startScan(ctx: Context): Long {
        val now = System.currentTimeMillis()
        val wait = 30_000 - (now - lastScanAt)
        if (wait > 0) return wait
        lastScanAt = now
        try { wm(ctx).startScan() } catch (_: Exception) {}
        return 0
    }

    /** Сети вокруг — последний скан (сам скан — startScan). */
    @Suppress("DEPRECATION")
    fun scanJson(ctx: Context): String {
        val o = head(ctx)
        if (!o.getBoolean("available")) return o.toString()
        val w = wm(ctx)
        val now = System.currentTimeMillis()
        val cur = try { w.connectionInfo?.bssid?.lowercase() } catch (_: Exception) { null }
        val list = JSONArray()
        val results = try { w.scanResults } catch (_: SecurityException) { emptyList() }
        for (r in results) {
            val n = JSONObject()
                .put("ssid", ssidOf(r))
                .put("bssid", (r.BSSID ?: "").lowercase())
                .put("level", r.level)
                .put("freq", r.frequency)
                .put("width", width(r))
                .put("sec", r.capabilities ?: "")
                .put("ageMs", (android.os.SystemClock.elapsedRealtime() - r.timestamp / 1000).coerceAtLeast(0))
            if (Build.VERSION.SDK_INT >= 30) n.put("std", r.wifiStandard)
            if (cur != null && cur == n.getString("bssid")) n.put("current", true)
            list.put(n)
        }
        return o.put("networks", list).put("nextScanMs", (30_000 - (now - lastScanAt)).coerceAtLeast(0)).toString()
    }

    /** Своя сеть сейчас: уровень обновляется сам, без скана. */
    @Suppress("DEPRECATION")
    fun infoJson(ctx: Context): String {
        val o = head(ctx)
        if (!o.getBoolean("available")) return o.toString()
        val i = try { wm(ctx).connectionInfo } catch (_: Exception) { null } ?: return o.put("connected", false).toString()
        if (i.networkId == -1 && (i.bssid == null || i.bssid == "02:00:00:00:00:00")) return o.put("connected", false).toString()
        o.put("connected", true)
            .put("ssid", (i.ssid ?: "").trim('"').takeIf { it != "<unknown ssid>" } ?: "")
            .put("bssid", (i.bssid ?: "").lowercase())
            .put("level", i.rssi)
            .put("freq", i.frequency)
            .put("rate", i.linkSpeed)
        if (Build.VERSION.SDK_INT >= 29) o.put("tx", i.txLinkSpeedMbps).put("rx", i.rxLinkSpeedMbps)
        if (Build.VERSION.SDK_INT >= 30) o.put("std", i.wifiStandard)
        return o.toString()
    }
}
