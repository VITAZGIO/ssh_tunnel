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
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Canvas
import android.graphics.Color
import android.graphics.ImageDecoder
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
import java.nio.ByteBuffer

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
                lastKey = "text:$text"
                ClipData.newPlainText("ssh_tunnel", text)
            } else {
                val uri = FileProvider.getUriForFile(c, c.packageName + ".files", File(path))
                lastKey = "uri:$uri"
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

    /** Итог отправки: ok — дошло хоть до одного устройства; final — повторять
     * бессмысленно (слишком большой), значит не пробовать снова при каждом
     * возврате в программу. */
    private class Res(val ok: Boolean, val msg: String, val final: Boolean = false)

    /** Что уходит по сети: kind — text/image/file; image — всегда PNG. */
    private class Prepared(val kind: String, val name: String, val mime: String, val data: ByteArray)

    private fun startsWith(d: ByteArray, vararg b: Int): Boolean =
        d.size >= b.size && b.indices.all { d[it] == b[it].toByte() }

    private val CONVERT_EXT = setOf("heic", "heif", "webp", "avif", "bmp", "tif", "tiff", "dng", "ico")

    /**
     * ТЗ 4: что отправить. Формат определяем по содержимому, а не по Mime
     * (галерея часто отдаёт пустой или неверный). PNG — как есть, вид image;
     * JPEG и GIF — как есть, вид file с Mime картинки (ПК положит в буфер
     * и картинкой, и файлом). Остальные картинки (HEIC/HEIF, WebP и пр.) ПК
     * разобрать не умеет — переводим в JPEG; не вышло — уходит как файл.
     */
    private fun prepare(name: String, mime: String, data: ByteArray): Prepared {
        if (startsWith(data, 0x89, 0x50, 0x4E, 0x47)) return Prepared("image", name, "image/png", data)
        if (startsWith(data, 0xFF, 0xD8, 0xFF)) return Prepared("file", name, "image/jpeg", data)
        if (startsWith(data, 0x47, 0x49, 0x46, 0x38)) return Prepared("file", name, "image/gif", data)
        val ext = name.substringAfterLast('.', "").lowercase()
        if (mime.startsWith("image/") || ext in CONVERT_EXT) {
            toJpeg(data)?.let {
                return Prepared("file", name.substringBeforeLast('.', name) + ".jpg", "image/jpeg", it)
            }
        }
        return Prepared("file", name, mime, data)
    }

    /** Любая картинка, которую умеет разобрать телефон → JPEG (поворот по EXIF
     * учитывается, прозрачность — на белом). null — не картинка / не хватило памяти. */
    private fun toJpeg(data: ByteArray): ByteArray? = try {
        val src = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            ImageDecoder.decodeBitmap(ImageDecoder.createSource(ByteBuffer.wrap(data))) { dec, _, _ ->
                dec.allocator = ImageDecoder.ALLOCATOR_SOFTWARE
            }
        } else {
            BitmapFactory.decodeByteArray(data, 0, data.size)
        }
        if (src == null) null else {
            val flat = Bitmap.createBitmap(src.width, src.height, Bitmap.Config.ARGB_8888)
            val cv = Canvas(flat)
            cv.drawColor(Color.WHITE)
            cv.drawBitmap(src, 0f, 0f, null)
            src.recycle()
            val out = java.io.ByteArrayOutputStream()
            flat.compress(Bitmap.CompressFormat.JPEG, 92, out)
            flat.recycle()
            out.toByteArray().takeIf { it.isNotEmpty() }
        }
    } catch (_: Throwable) {
        null
    }

    /** Файл/картинка по uri → отправка. Общий путь для «Поделиться», плитки,
     * кнопки в уведомлении, службы спецвозможностей и автоотправки. */
    private fun pushUri(ctx: Context, uri: Uri): Res {
        val r = readUri(ctx, uri) ?: return Res(false, ctx.getString(R.string.clip_empty))
        if (r.third.size > MAX) return Res(false, ctx.getString(R.string.clip_too_big), true)
        val p = prepare(r.first, r.second, r.third)
        val (ok, msg) = pushResult(ctx, p.kind, p.name, p.mime, p.data)
        return Res(ok, msg)
    }

    private fun pushText(ctx: Context, text: String): Res {
        val (ok, msg) = pushResult(ctx, "text", "", "", text.toByteArray())
        return Res(ok, msg)
    }

    /** Отправить ClipData (из «Поделиться» или буфера) в фоне и показать итог.
     * key — отпечаток элемента буфера (см. keyOf): при успехе его запоминаем,
     * чтобы автоотправка не слала то же самое второй раз. */
    fun sendClip(a: Activity, text: CharSequence?, uri: Uri?, key: String? = null) {
        val ctx = a.applicationContext
        Thread {
            val res = when {
                uri != null -> pushUri(ctx, uri)
                !text.isNullOrEmpty() -> pushText(ctx, text.toString())
                else -> Res(false, ctx.getString(R.string.clip_empty))
            }
            if (res.ok || res.final) key?.let { lastKey = it }
            main.post { Toast.makeText(ctx, res.msg, Toast.LENGTH_SHORT).show() }
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
                val res = pushUri(ctx, uri)
                lastMsg = res.msg
                if (res.ok) ok++
            }
            val msg = if (uris.size <= 1 || ok == 0) lastMsg
                else ctx.getString(R.string.clip_sent_multi, ok, uris.size)
            main.post { Toast.makeText(ctx, msg, Toast.LENGTH_SHORT).show() }
        }.start()
    }

    const val MAX = 50 * 1024 * 1024

    /** Отпечаток последнего элемента буфера, который реально ушёл (или который
     * положили мы сами): "text:…" или "uri:…". Его не шлём снова. Если отправка
     * не удалась (нет сети — «0 устройств»), отпечаток остаётся прежним, и как
     * только появится связь, элемент уйдёт. */
    @Volatile private var lastKey: String? = null

    /** Отпечаток, который отправляется прямо сейчас (не запускать второй раз). */
    @Volatile private var sending: String? = null

    /** Кого видели последним в фокусе — чтобы повторить отправку, когда
     * вернётся сеть, даже без повторного переключения на программу. */
    @Volatile private var lastActivity: java.lang.ref.WeakReference<Activity>? = null

    /** Текст элемента буфера; ссылки content:// и intent-элементы текстом не
     * считаются (иначе адрес картинки ушёл бы на ПК строкой). */
    fun textOf(ctx: Context, item: ClipData.Item): String? {
        if (item.uri != null) return null
        val t = item.text ?: if (item.htmlText != null) item.coerceToText(ctx) else null
        return t?.toString()?.takeIf { it.isNotEmpty() }
    }

    /** Отпечаток элемента буфера; null — отправлять нечего или это наша же
     * запись (файл, который только что принят с другого устройства). */
    fun keyOf(ctx: Context, item: ClipData.Item): String? {
        val uri = item.uri
        if (uri != null) {
            if (uri.authority == ctx.packageName + ".files") return null
            return "uri:$uri"
        }
        return textOf(ctx, item)?.let { "text:$it" }
    }

    private fun primaryItem(a: Activity): ClipData.Item? = try {
        a.getSystemService(ClipboardManager::class.java)?.primaryClip?.takeIf { it.itemCount > 0 }?.getItemAt(0)
    } catch (_: Exception) {
        null
    }

    /**
     * Буфер телефона → компьютеры без кнопок. Android (10+) даёт читать буфер
     * только программе на экране, поэтому скопированное в другом приложении
     * уходит, как только открываешь ssh_tunnel. Текст и картинки/файлы идут
     * одним путём с «Поделиться» и плиткой. Плагин не добавлен или отправка
     * выключена — ядро отвечает ошибкой, и молчим.
     */
    fun autoSend(a: Activity) {
        lastActivity = java.lang.ref.WeakReference(a)
        val item = primaryItem(a) ?: return
        val key = keyOf(a, item) ?: return
        if (key == lastKey || key == sending) return
        send(a.applicationContext, item, key)
    }

    /** Пытается заново отправить то, что в буфере, когда появилась сеть —
     * без этого «0 устройств» офлайн так и оставалось висеть недошедшим. */
    fun retrySend() {
        val a = lastActivity?.get() ?: return
        val item = primaryItem(a) ?: return
        val key = keyOf(a, item) ?: return
        if (key == lastKey || key == sending) return
        send(a.applicationContext, item, key)
    }

    private fun send(ctx: Context, item: ClipData.Item, key: String) {
        val uri = item.uri
        val text = if (uri == null) textOf(ctx, item) else null
        sending = key
        Thread {
            try {
                DeviceApps.init(ctx)
                val res = when {
                    uri != null -> pushUri(ctx, uri)
                    text != null -> pushText(ctx, text)
                    else -> return@Thread
                }
                // Только при успехе считаем элемент «уже отправленным» — иначе
                // при восстановлении сети повторить будет нечего (см. retrySend).
                // Слишком большой тоже запоминаем: иначе каждый возврат в
                // программу заново читал бы до 50 МБ.
                if (res.ok || res.final) {
                    lastKey = key
                    main.post { Toast.makeText(ctx, res.msg, Toast.LENGTH_SHORT).show() }
                }
            } finally {
                sending = null
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
        val text = item?.let { Tools5.textOf(this, it) }
        Tools5.sendClip(this, text, uri, item?.let { Tools5.keyOf(this, it) })
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
