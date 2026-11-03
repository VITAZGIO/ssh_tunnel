package io.github.vitazgio.sshtunnel

import android.app.Activity
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.ClipData
import android.content.ClipDescription
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.graphics.drawable.Icon
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.OpenableColumns
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService
import android.system.OsConstants
import android.widget.Toast
import androidx.core.content.FileProvider
import org.json.JSONObject
import java.io.File
import java.net.Inet4Address
import java.net.InetAddress
import java.net.InetSocketAddress

/**
 * ТЗ 5: Расход трафика, Сканер сети, Общий буфер — то, что ядру на Go даёт
 * только телефон (mobile.Phone):
 *
 * - чьё соединение — ConnectivityManager.getConnectionOwnerUid (Android 10+,
 *   разрешено только службе VPN), uid → пакет;
 * - какая сеть под VPN — Wi-Fi или мобильная (для деления расхода);
 * - подсеть Wi-Fi для сканера (net.Interfaces в Go на Android 11+ не
 *   работает: netlink закрыт);
 * - системный буфер: принять — сразу (класть в буфер из фона можно), отдать —
 *   только «Поделиться» и плитка (читать буфер в фоне Android не даёт).
 */
object Tools5 : mobile.Phone {
    @Volatile private var app: Context? = null
    private val main = Handler(Looper.getMainLooper())

    fun init(ctx: Context) {
        if (app != null) return
        val c = ctx.applicationContext
        app = c
        mobile.Mobile.setPhone(this)
        watchNetwork(c)
    }

    private fun cm(): ConnectivityManager? = app?.getSystemService(ConnectivityManager::class.java)

    /** Сеть под VPN: Wi-Fi или мобильная. Меняется — сообщаем ядру. */
    private fun watchNetwork(c: Context) {
        val cm = c.getSystemService(ConnectivityManager::class.java) ?: return
        val update = { mobile.Mobile.setNetworkKind(kind(cm)) }
        update()
        try {
            cm.registerNetworkCallback(
                android.net.NetworkRequest.Builder()
                    .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                    .removeCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
                    .build(),
                object : ConnectivityManager.NetworkCallback() {
                    override fun onAvailable(network: Network) = update()
                    override fun onLost(network: Network) = update()
                    override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) = update()
                },
            )
        } catch (_: Exception) {
        }
    }

    /** Настоящие сети (не VPN), Wi-Fi/Ethernet — первыми. */
    private fun underlying(cm: ConnectivityManager): List<Pair<Network, NetworkCapabilities>> {
        @Suppress("DEPRECATION")
        val all = cm.allNetworks.mapNotNull { n -> cm.getNetworkCapabilities(n)?.let { n to it } }
            .filter { !it.second.hasTransport(NetworkCapabilities.TRANSPORT_VPN) }
        return all.sortedBy {
            if (it.second.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) ||
                it.second.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) 0 else 1
        }
    }

    private fun kind(cm: ConnectivityManager): String {
        val caps = underlying(cm).firstOrNull { it.second.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) }?.second
            ?: return ""
        return when {
            caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) ||
                caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "wifi"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "mobile"
            else -> ""
        }
    }

    override fun ownerOf(proto: Long, srcIP: String, srcPort: Long, dstIP: String, dstPort: Long): String {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) return ""
        val cm = cm() ?: return ""
        return try {
            val local = InetSocketAddress(InetAddress.getByName(srcIP), srcPort.toInt())
            val remote = if (dstIP.isEmpty()) InetSocketAddress(0) else InetSocketAddress(InetAddress.getByName(dstIP), dstPort.toInt())
            val p = if (proto == 17L) OsConstants.IPPROTO_UDP else OsConstants.IPPROTO_TCP
            val uid = cm.getConnectionOwnerUid(p, local, remote)
            if (uid < 0) "" else app?.packageManager?.getPackagesForUid(uid)?.firstOrNull() ?: "uid:$uid"
        } catch (_: Exception) {
            ""
        }
    }

    override fun localNet(): String {
        val cm = cm() ?: return ""
        for ((n, caps) in underlying(cm)) {
            if (!caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) &&
                !caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) continue
            val lp = cm.getLinkProperties(n) ?: continue
            val a = lp.linkAddresses.firstOrNull { it.address is Inet4Address } ?: continue
            return "${a.address.hostAddress}/${a.prefixLength}"
        }
        return ""
    }

    override fun onClip(kind: String, text: String, path: String, mime: String, name: String) {
        val c = app ?: return
        main.post {
            val cb = c.getSystemService(ClipboardManager::class.java) ?: return@post
            val clip = if (kind == "text") {
                ClipData.newPlainText("ssh_tunnel", text)
            } else {
                val uri = FileProvider.getUriForFile(c, c.packageName + ".files", File(path))
                val type = mime.ifEmpty { if (kind == "image") "image/png" else "application/octet-stream" }
                ClipData(ClipDescription(name.ifEmpty { "ssh_tunnel" }, arrayOf(type)), ClipData.Item(uri))
            }
            try {
                cb.setPrimaryClip(clip)
            } catch (_: Exception) {
            }
        }
    }

    override fun alert(text: String) {
        val c = app ?: return
        val nm = c.getSystemService(NotificationManager::class.java) ?: return
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            nm.createNotificationChannel(
                NotificationChannel("tools", c.getString(R.string.tools_channel), NotificationManager.IMPORTANCE_DEFAULT)
            )
        }
        val n = Notification.Builder(c, "tools")
            .setSmallIcon(R.drawable.ic_notify)
            .setContentTitle(c.getString(R.string.app_name))
            .setContentText(text)
            .setStyle(Notification.BigTextStyle().bigText(text))
            .setAutoCancel(true)
            .build()
        try {
            nm.notify(4015, n)
        } catch (_: SecurityException) {
        }
    }

    /** Отправить всем устройствам; ответ — фраза для Toast. */
    fun push(ctx: Context, kind: String, name: String, mime: String, data: ByteArray): String {
        DeviceApps.init(ctx)
        val r = try {
            JSONObject(mobile.Mobile.clipPush(kind, name, mime, data))
        } catch (e: Exception) {
            return e.message ?: "?"
        }
        return when {
            r.optBoolean("tooBig") -> ctx.getString(R.string.clip_too_big)
            r.has("error") -> r.optString("error")
            else -> ctx.getString(R.string.clip_sent, r.optInt("sent"))
        }
    }

    /** Содержимое по uri (картинка/файл) — не больше 50 МБ + 1 байт. */
    fun readUri(ctx: Context, uri: Uri): Triple<String, String, ByteArray>? {
        val cr = ctx.contentResolver
        var name = "file"
        try {
            cr.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use {
                if (it.moveToFirst()) name = it.getString(0) ?: name
            }
        } catch (_: Exception) {
        }
        val mime = cr.getType(uri) ?: ""
        val data = try {
            cr.openInputStream(uri)?.use { s ->
                val out = java.io.ByteArrayOutputStream()
                val buf = ByteArray(64 * 1024)
                while (true) {
                    val n = s.read(buf)
                    if (n < 0) break
                    out.write(buf, 0, n)
                    if (out.size() > MAX) break
                }
                out.toByteArray()
            }
        } catch (_: Exception) {
            null
        } ?: return null
        return Triple(name, mime, data)
    }

    /** Отправить ClipData (из «Поделиться» или буфера) в фоне и показать итог. */
    fun sendClip(a: Activity, text: CharSequence?, uri: Uri?) {
        val ctx = a.applicationContext
        Thread {
            val msg = when {
                uri != null -> {
                    val r = readUri(ctx, uri)
                    when {
                        r == null -> ctx.getString(R.string.clip_empty)
                        r.third.size > MAX -> ctx.getString(R.string.clip_too_big)
                        else -> {
                            val image = r.second == "image/png"
                            push(ctx, if (image) "image" else "file", r.first, r.second, r.third)
                        }
                    }
                }
                !text.isNullOrEmpty() -> push(ctx, "text", "", "", text.toString().toByteArray())
                else -> ctx.getString(R.string.clip_empty)
            }
            main.post { Toast.makeText(ctx, msg, Toast.LENGTH_SHORT).show() }
        }.start()
    }

    const val MAX = 50 * 1024 * 1024
}

/** «Поделиться → ssh_tunnel»: текст, картинка или файл — всем устройствам. Окна нет. */
class ClipShareActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val i = intent
        @Suppress("DEPRECATION")
        val uri = if (Build.VERSION.SDK_INT >= 33) i.getParcelableExtra(Intent.EXTRA_STREAM, Uri::class.java)
        else i.getParcelableExtra(Intent.EXTRA_STREAM) as? Uri
        Tools5.sendClip(this, i.getCharSequenceExtra(Intent.EXTRA_TEXT), uri)
        finish()
    }
}

/**
 * Плитка буфера открывает это прозрачное окно: читать буфер Android разрешает
 * только приложению с фокусом — поэтому ждём фокус, читаем и закрываемся.
 */
class ClipReadActivity : Activity() {
    private var done = false

    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (!hasFocus || done) return
        done = true
        val cb = getSystemService(ClipboardManager::class.java)
        val item = cb?.primaryClip?.takeIf { it.itemCount > 0 }?.getItemAt(0)
        val uri = item?.uri
        val text = item?.text ?: if (uri == null) item?.coerceToText(this) else null
        Tools5.sendClip(this, text, uri)
        finish()
    }
}

/** Плитка «Общий буфер» в шторке: отправить то, что сейчас в буфере телефона. */
class ClipTile : TileService() {
    override fun onStartListening() {
        super.onStartListening()
        val t = qsTile ?: return
        t.state = Tile.STATE_INACTIVE
        t.label = getString(R.string.clip_tile)
        t.icon = Icon.createWithResource(this, R.drawable.ic_clip)
        t.updateTile()
    }

    override fun onClick() {
        super.onClick()
        val intent = Intent(this, ClipReadActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startActivityAndCollapse(
                PendingIntent.getActivity(this, 1, intent, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
            )
        } else {
            @Suppress("DEPRECATION")
            startActivityAndCollapse(intent)
        }
    }
}
