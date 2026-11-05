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
        watchBattery(c)
    }

    /** Заряд — другим устройствам («Связь с телефоном»), при каждом изменении процента. */
    private var lastBattery = -1
    private fun watchBattery(c: Context) {
        val f = android.content.IntentFilter(Intent.ACTION_BATTERY_CHANGED)
        val r = object : android.content.BroadcastReceiver() {
            override fun onReceive(ctx: Context, i: Intent) {
                val lvl = i.getIntExtra(android.os.BatteryManager.EXTRA_LEVEL, -1)
                val scale = i.getIntExtra(android.os.BatteryManager.EXTRA_SCALE, 100)
                if (lvl < 0 || scale <= 0) return
                val pct = lvl * 100 / scale
                val st = i.getIntExtra(android.os.BatteryManager.EXTRA_STATUS, 0)
                val charging = st == android.os.BatteryManager.BATTERY_STATUS_CHARGING || st == android.os.BatteryManager.BATTERY_STATUS_FULL
                if (pct == lastBattery) return
                lastBattery = pct
                Thread { mobile.Mobile.phoneLinkBattery(pct.toLong(), charging) }.start()
            }
        }
        try {
            if (Build.VERSION.SDK_INT >= 33) c.registerReceiver(r, f, Context.RECEIVER_NOT_EXPORTED) else c.registerReceiver(r, f)
        } catch (_: Exception) {
        }
    }

    /** «Найти телефон»: свой трек или будильник, нарастает до максимума (FindRing). */
    override fun ring() {
        val c = app ?: return
        FindRing.init(c)
        FindRing.start(c)
    }

    /** «Стоп» поиска с другого устройства. */
    override fun stopRing() {
        FindRing.stop()
    }

    /** Медиа-кнопка с другого устройства: играет/пауза/дальше/громкость. */
    override fun media(key: String) {
        val c = app ?: return
        val am = c.getSystemService(android.media.AudioManager::class.java) ?: return
        val code = when (key) {
            "playpause" -> android.view.KeyEvent.KEYCODE_MEDIA_PLAY_PAUSE
            "next" -> android.view.KeyEvent.KEYCODE_MEDIA_NEXT
            "prev" -> android.view.KeyEvent.KEYCODE_MEDIA_PREVIOUS
            "stop" -> android.view.KeyEvent.KEYCODE_MEDIA_STOP
            else -> 0
        }
        if (code != 0) {
            am.dispatchMediaKeyEvent(android.view.KeyEvent(android.view.KeyEvent.ACTION_DOWN, code))
            am.dispatchMediaKeyEvent(android.view.KeyEvent(android.view.KeyEvent.ACTION_UP, code))
            return
        }
        val dir = when (key) {
            "volup" -> android.media.AudioManager.ADJUST_RAISE
            "voldown" -> android.media.AudioManager.ADJUST_LOWER
            "mute" -> android.media.AudioManager.ADJUST_TOGGLE_MUTE
            else -> return
        }
        am.adjustStreamVolume(android.media.AudioManager.STREAM_MUSIC, dir, android.media.AudioManager.FLAG_SHOW_UI)
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
                    override fun onAvailable(network: Network) { update(); retrySend() }
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
                lastText = text
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
    fun push(ctx: Context, kind: String, name: String, mime: String, data: ByteArray): String =
        pushResult(ctx, kind, name, mime, data).second

    /** То же, но вместе с тем, дошло ли хоть до одного устройства — строку
     * для тоста разбирать на «успех/нет» ненадёжно (она уже переведена). */
    private fun pushResult(ctx: Context, kind: String, name: String, mime: String, data: ByteArray): Pair<Boolean, String> {
        DeviceApps.init(ctx)
        val r = try {
            JSONObject(mobile.Mobile.clipPush(kind, name, mime, data))
        } catch (e: Exception) {
            return false to (e.message ?: "?")
        }
        return when {
            r.optBoolean("tooBig") -> false to ctx.getString(R.string.clip_too_big)
            r.has("error") -> false to r.optString("error")
            else -> {
                val n = r.optInt("sent")
                (n > 0) to ctx.getString(R.string.clip_sent, n)
            }
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

    /** То же самое, но для нескольких файлов за раз (галерея: выделил
     * несколько и «Поделиться» → ACTION_SEND_MULTIPLE). Каждый уходит своим
     * вызовом clipPush по очереди — протокол его на пачку не рассчитан;
     * итог — одно общее сообщение, а не тост на файл. */
    fun sendClipMultiple(a: Activity, uris: List<Uri>) {
        val ctx = a.applicationContext
        Thread {
            var ok = 0
            var lastMsg = ""
            for (uri in uris) {
                val r = readUri(ctx, uri)
                val (success, msg) = when {
                    r == null -> false to ctx.getString(R.string.clip_empty)
                    r.third.size > MAX -> false to ctx.getString(R.string.clip_too_big)
                    else -> {
                        val image = r.second == "image/png"
                        pushResult(ctx, if (image) "image" else "file", r.first, r.second, r.third)
                    }
                }
                lastMsg = msg
                if (success) ok++
            }
            val msg = if (uris.size <= 1 || ok == 0) lastMsg
                else ctx.getString(R.string.clip_sent_multi, ok, uris.size)
            main.post { Toast.makeText(ctx, msg, Toast.LENGTH_SHORT).show() }
        }.start()
    }

    const val MAX = 50 * 1024 * 1024

    /** Последний текст буфера, который реально ушёл (sent &gt; 0) или пришёл:
     * его не шлём снова. Если отправка не удалась (нет сети — «0 устройств»),
     * lastText остаётся прежним, и как только появится связь, текст уйдёт. */
    @Volatile private var lastText: String? = null

    /** Кого видели последним в фокусе — чтобы повторить отправку, когда
     * вернётся сеть, даже без повторного переключения на программу. */
    @Volatile private var lastActivity: java.lang.ref.WeakReference<Activity>? = null

    /**
     * Буфер телефона → компьютеры без кнопок. Android (10+) даёт читать буфер
     * только программе на экране, поэтому скопированное в другом приложении
     * уходит, как только открываешь ssh_tunnel. Плагин не добавлен или
     * отправка выключена — ядро отвечает ошибкой, и молчим.
     */
    fun autoSend(a: Activity) {
        lastActivity = java.lang.ref.WeakReference(a)
        val cb = a.getSystemService(ClipboardManager::class.java) ?: return
        val text = try {
            cb.primaryClip?.takeIf { it.itemCount > 0 }?.getItemAt(0)?.coerceToText(a)?.toString()
        } catch (_: Exception) {
            null
        }
        if (text.isNullOrEmpty() || text == lastText) return
        send(a.applicationContext, text)
    }

    /** Пытается заново отправить то, что в буфере, когда появилась сеть —
     * без этого «0 устройств» офлайн так и оставалось висеть недошедшим. */
    fun retrySend() {
        val a = lastActivity?.get() ?: return
        val cb = a.getSystemService(ClipboardManager::class.java) ?: return
        val text = try {
            cb.primaryClip?.takeIf { it.itemCount > 0 }?.getItemAt(0)?.coerceToText(a)?.toString()
        } catch (_: Exception) {
            null
        }
        if (text.isNullOrEmpty() || text != lastText) return
        send(a.applicationContext, text)
    }

    private fun send(ctx: Context, text: String) {
        Thread {
            DeviceApps.init(ctx)
            val r = try {
                JSONObject(mobile.Mobile.clipPush("text", "", "", text.toByteArray()))
            } catch (_: Exception) {
                return@Thread
            }
            val n = r.optInt("sent")
            // Только при успехе считаем текст «уже отправленным» — иначе при
            // восстановлении сети повторить будет нечего (см. retrySend).
            if (n > 0) {
                lastText = text
                main.post { Toast.makeText(ctx, ctx.getString(R.string.clip_sent, n), Toast.LENGTH_SHORT).show() }
            }
        }.start()
    }
}

/** «Поделиться → ssh_tunnel»: текст, картинка или файл — всем устройствам. Окна нет. */
class ClipShareActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val i = intent
        if (i.action == Intent.ACTION_SEND_MULTIPLE) {
            @Suppress("DEPRECATION")
            val uris = if (Build.VERSION.SDK_INT >= 33) i.getParcelableArrayListExtra(Intent.EXTRA_STREAM, Uri::class.java)
            else i.getParcelableArrayListExtra(Intent.EXTRA_STREAM)
            if (!uris.isNullOrEmpty()) Tools5.sendClipMultiple(this, uris)
        } else {
            @Suppress("DEPRECATION")
            val uri = if (Build.VERSION.SDK_INT >= 33) i.getParcelableExtra(Intent.EXTRA_STREAM, Uri::class.java)
            else i.getParcelableExtra(Intent.EXTRA_STREAM) as? Uri
            Tools5.sendClip(this, i.getCharSequenceExtra(Intent.EXTRA_TEXT), uri)
        }
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
