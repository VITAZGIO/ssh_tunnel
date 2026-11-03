package io.github.vitazgio.sshtunnel

import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.Canvas
import android.net.Uri
import android.net.VpnService
import android.os.Handler
import android.os.Looper
import android.util.Base64
import org.json.JSONArray
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.lang.ref.WeakReference
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.TimeZone

/**
 * API главного экрана на телефоне — те же адреса и тот же JSON, что у окна ПК
 * (src/internal/webui, docs/API.md). Общий index.html ходит сюда через
 * сервер страниц ядра (mobile.StartUI → mobile.SetAPI), поэтому экран один
 * на обе платформы. Здесь только перевод запросов на язык телефона:
 * настройки (Settings), VPN-служба (TunnelService), список приложений.
 * Логику «куда» здесь не решаем — её решает ядро в службе.
 *
 * События экрану (поток /events) — mobile.Emit: core (две кнопки), stats,
 * log — в том же виде, что на ПК.
 */
object AndroidApi : mobile.API {

    private lateinit var app: Context
    private val main = Handler(Looper.getMainLooper())

    /** Открытый экран: системный вопрос о VPN задаётся только с него. */
    interface Screen {
        fun askVpnPermission()
    }

    @Volatile var screen: WeakReference<Screen>? = null

    // Ошибка, которую ядро в службе не видит (не дали разрешение на VPN,
    // не заполнены настройки): показывается на круглой кнопке до следующего
    // нажатия.
    @Volatile private var localError = ""

    // Служба запускается не мгновенно: пока она не сообщила о себе, кнопка
    // уже «Подключаюсь…», а не выключена.
    @Volatile private var startingUntil = 0L
    @Volatile private var startingMesh = false
    @Volatile private var startingBypass = false

    private var seq = 0L
    private var lastCore = ""

    @Synchronized
    fun init(ctx: Context): Boolean {
        if (::app.isInitialized) return true
        app = ctx.applicationContext
        return try {
            mobile.Mobile.startUI(app.filesDir.absolutePath)
            mobile.Mobile.setAPI(this)
            TunnelService.onApiUpdate = { pushState() }
            TunnelService.onApiLog = { line -> emitLog(line) }
            true
        } catch (e: Exception) {
            false
        }
    }

    /** Адрес главного экрана для WebView. */
    fun mainUrl(): String {
        val s = Settings(app)
        val q = Uri.Builder()
            .appendQueryParameter("t", mobile.Mobile.uiToken())
            .appendQueryParameter("lang", if (s.language == "en") "en" else "ru")
        if (s.theme.isNotEmpty()) q.appendQueryParameter("theme", s.theme)
        return mobile.Mobile.startUI(app.filesDir.absolutePath) + "/index.html" + q.build().toString()
    }

    override fun handle(method: String, path: String, query: String, body: String): String = try {
        route(method, path, Uri.parse("http://local/?$query"), body)
    } catch (e: Exception) {
        err(e.message ?: e.toString())
    }

    private fun route(method: String, path: String, q: Uri, body: String): String {
        val s = Settings(app)
        val post = method == "POST"
        return when (path) {
            "/api" -> JSONObject().put("api", 1).put("platform", "android").toString()
            "/api/status" -> status(s).toString()
            "/api/state" -> {
                if (post) {
                    val o = obj(body)
                    want(s, if (o.has("tunnel")) o.getBoolean("tunnel") else null,
                        if (o.has("mesh")) o.getBoolean("mesh") else null)?.let {
                        return JSONObject().put("error", it).put("state", core(s)).toString()
                    }
                }
                core(s).toString()
            }
            "/api/start" -> wantResult(s, true, null)
            "/api/stop" -> wantResult(s, false, false)
            "/api/bypass" -> wantResult(s, obj(body).optBoolean("on", true), null)
            "/api/meshmode" -> wantResult(s, null, obj(body).optBoolean("on", true))
            "/api/config" -> if (post) saveConfig(s, obj(body)) else config(s).toString()
            "/api/profile/add" -> {
                val o = obj(body)
                val p = s.addProfile(o.optString("name"), o.optString("flag"))
                JSONObject().put("profile", profileJson(s, p)).put("config", config(s)).toString()
            }
            "/api/profile/remove" -> removeProfile(s, obj(body).optString("id"))
            "/api/profile/select" -> selectProfile(s, obj(body).optString("id"))
            "/api/profile/export" -> exportProfile(s, obj(body))
            "/api/profile/import" -> importText(s, body)
            "/api/profile/latency" -> latency(s)
            "/api/key" -> saveKey(s, obj(body))
            "/api/settings/export" -> {
                val list = s.profiles
                JSONObject().put("data", SettingsBundle.build(s)).put("servers", list.size)
                    .put("keysMissing", list.count { !hasKey(s, it.id) }).toString()
            }
            "/api/settings/import" -> {
                val n = try {
                    SettingsBundle.apply(s, body)
                } catch (e: SettingsBundle.BadBundle) {
                    return err(app.getString(R.string.bundle_bad))
                }
                restartIfRunning(s)
                JSONObject().put("config", config(s)).put("servers", n)
                    .put("keysMissing", s.profiles.count { !hasKey(s, it.id) }).toString()
            }
            "/api/appprofiles" -> JSONObject().put("state", appProfilesState(AppProfiles(app, s))).toString()
            "/api/appprofiles/enable", "/api/appprofiles/add",
            "/api/appprofiles/switch", "/api/appprofiles/delete" -> appProfileAction(s, path, obj(body))
            "/api/mesh" -> TunnelService.meshStatus()
            "/api/checkip" -> TunnelService.checkIP()
            "/api/speedtest" -> TunnelService.speedTest()
            "/api/selfcheck" -> TunnelService.selfCheck(app)
            "/api/serverusers" -> {
                val o = obj(body)
                TunnelService.serverUsers(o.optString("op"), o.optString("name"),
                    o.optString("deviceType"), o.optString("password"))
            }
            "/api/adblock/update" -> {
                val r = mobile.Mobile.updateBlockLists(obj(body).optString("sources"), s.adBlockListFile.absolutePath)
                pushState()
                r
            }
            "/api/update/check" -> updateCheck()
            "/api/update/download" -> updateDownload()
            "/api/processes" -> apps()
            "/api/appicon" -> appIcon(q.getQueryParameter("pkg") ?: q.getQueryParameter("path") ?: "")
            else -> err(app.getString(R.string.api_not_on_phone))
        }
    }

    // ---------------------------------------------------------------------
    // Две кнопки
    // ---------------------------------------------------------------------

    private fun running() = TunnelService.state.let { it == "connecting" || it == "connected" || it == "reconnecting" }

    /** Состояние ядра (core.State) из того, что знает служба. */
    private fun core(s: Settings): JSONObject {
        val st = TunnelService.state
        val run = running()
        val meshOk = s.meshConfigured()
        val starting = !run && st != "error" && System.currentTimeMillis() < startingUntil
        val bypass = if (starting) startingBypass else run && TunnelService.bypassOn
        val mesh = meshOk && (if (starting) startingMesh else run && TunnelService.meshOn)
        val linkUp = st == "connected"
        fun phase(on: Boolean) = when {
            !on -> "off"
            starting -> "connecting"
            linkUp || st == "reconnecting" -> "on"
            else -> "connecting"
        }
        val tunnel = JSONObject().put("want", bypass).put("phase", phase(bypass))
        val err = localError.ifEmpty { if (st == "error") TunnelService.detail else "" }
        if (err.isNotEmpty() && !bypass && !mesh) tunnel.put("phase", "error").put("error", err)
        val m = JSONObject().put("want", mesh).put("phase", phase(mesh))
        val mode = when {
            bypass && mesh -> "bypass+mesh"
            bypass -> "bypass"
            mesh -> "mesh"
            else -> "off"
        }
        val link = if (starting) "connecting" else st
        return JSONObject().put("tunnel", tunnel).put("mesh", m).put("meshAvailable", meshOk)
            .put("mode", mode).put("link", link)
            .put("detail", if (linkUp) s.active().host else "")
            .put("seq", seq)
    }

    private fun wantResult(s: Settings, tunnel: Boolean?, mesh: Boolean?): String {
        want(s, tunnel, mesh)?.let { return err(it) }
        return JSONObject().put("ok", true).put("state", core(s)).toString()
    }

    /**
     * «Хочу» с экрана: tunnel/mesh — null, если эту кнопку не трогали.
     * Возвращает текст ошибки или null.
     */
    @Synchronized
    private fun want(s: Settings, tunnel: Boolean?, mesh: Boolean?): String? {
        localError = ""
        val run = running()
        val meshOk = s.meshConfigured()
        val curBypass = run && TunnelService.bypassOn
        val curMesh = run && meshOk && TunnelService.meshOn
        val b = tunnel ?: curBypass
        val m = meshOk && (mesh ?: curMesh)
        s.lastRunning = b
        s.lastMesh = m
        if (!b && !m) {
            startingUntil = 0
            if (TunnelService.state != "stopped") service(TunnelService.ACTION_STOP)
            pushState()
            return null
        }
        if (run) {
            service(TunnelService.ACTION_MODES, b, m)
            pushState()
            return null
        }
        if (!s.ready()) {
            localError = app.getString(R.string.need_settings)
            pushState()
            return localError
        }
        startingBypass = b
        startingMesh = m
        startingUntil = System.currentTimeMillis() + 20_000
        if (VpnService.prepare(app) != null) {
            // Разрешение на VPN спрашивает сама система — только с открытого
            // экрана. Ответ придёт в vpnAllowed.
            val a = screen?.get()
            if (a == null) {
                startingUntil = 0
                localError = app.getString(R.string.vpn_permission_needed)
                pushState()
                return localError
            }
            main.post { a.askVpnPermission() }
            pushState()
            return null
        }
        launch(b, m)
        return null
    }

    /** Ответ системы на вопрос о VPN (MainActivity). */
    fun vpnAllowed(ok: Boolean) {
        if (ok) {
            launch(startingBypass, startingMesh)
        } else {
            startingUntil = 0
            localError = app.getString(R.string.vpn_permission_denied)
            pushState()
        }
    }

    private fun launch(bypass: Boolean, mesh: Boolean) {
        if (bypass && mesh) service(TunnelService.ACTION_START) else service(TunnelService.ACTION_MODES, bypass, mesh)
        pushState()
    }

    private fun service(action: String, bypass: Boolean? = null, mesh: Boolean? = null) {
        val i = Intent(app, TunnelService::class.java).setAction(action)
        if (bypass != null) i.putExtra(TunnelService.EXTRA_BYPASS, bypass)
        if (mesh != null) i.putExtra(TunnelService.EXTRA_MESH, mesh)
        app.startService(i)
    }

    /** Режим при открытии приложения (Настройки → Запуск), как раньше. */
    fun applyStartMode() {
        val s = Settings(app)
        fun wanted(mode: String, last: Boolean) = when (mode) {
            "on" -> true
            "last" -> last
            else -> false
        }
        val bypass = wanted(s.startMode, s.lastRunning)
        val mesh = s.meshConfigured() && wanted(s.meshStartEffective, s.lastMesh)
        if ((bypass || mesh) && TunnelService.state == "stopped" && s.ready() && VpnService.prepare(app) == null) {
            launch(bypass, mesh)
        }
    }

    /** Служба что-то сообщила — экрану свежие кнопки и счётчики. */
    fun pushState() {
        if (!::app.isInitialized) return
        val s = Settings(app)
        if (running() || TunnelService.state == "error") startingUntil = 0
        synchronized(this) {
            val c = core(s)
            val key = c.toString()
            if (key != lastCore) {
                lastCore = key
                seq++
                c.put("seq", seq)
                mobile.Mobile.emit(JSONObject().put("kind", "core").put("core", c).toString())
            }
        }
        val stats = try { JSONObject(TunnelService.statsJson) } catch (_: Exception) { JSONObject() }
        if (stats.length() > 0) mobile.Mobile.emit(JSONObject().put("kind", "stats").put("stats", stats).toString())
    }

    private val isoTime = SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss.SSS'Z'", Locale.US).apply {
        timeZone = TimeZone.getTimeZone("UTC")
    }

    private fun emitLog(line: String) {
        val level = if (line.contains("ошибк", true) || line.contains("error", true)) "error" else "info"
        val time = synchronized(isoTime) { isoTime.format(Date()) }
        mobile.Mobile.emit(JSONObject().put("kind", "log").put("level", level).put("text", line)
            .put("time", time).toString())
    }

    private fun status(s: Settings): JSONObject {
        val stats = try { JSONObject(TunnelService.statsJson) } catch (_: Exception) { JSONObject() }
        val run = running()
        val st = if (System.currentTimeMillis() < startingUntil && !run) "connecting" else TunnelService.state
        return JSONObject()
            .put("state", st)
            .put("running", run)
            .put("config", config(s))
            .put("stats", stats)
            .put("os", "android")
            .put("effectiveProfile", s.activeProfileId)
            .put("version", BuildConfig.VERSION_NAME)
            .put("modes", JSONObject().put("bypass", run && TunnelService.bypassOn)
                .put("mesh", run && TunnelService.meshOn && s.meshConfigured())
                .put("meshConfigured", s.meshConfigured()))
            .put("adBlock", true)
            .put("adBlocked", s.adBlockTotal)
            .put("seenApps", JSONArray())
            .put("core", core(s))
    }

    // ---------------------------------------------------------------------
    // Настройки: тот же документ, что config.json на ПК
    // ---------------------------------------------------------------------

    private fun hasKey(s: Settings, id: String) = s.keyFile(id).let { it.exists() && it.length() > 0 }

    private fun lines(text: String) = JSONArray(text.split("\n").map { it.trim() }.filter { it.isNotEmpty() })

    private fun joinLines(a: JSONArray?): String =
        if (a == null) "" else (0 until a.length()).map { a.optString(it).trim() }.filter { it.isNotEmpty() }.joinToString("\n")

    private fun profileJson(s: Settings, p: Settings.Profile): JSONObject = JSONObject()
        .put("id", p.id).put("name", p.name).put("flag", p.flag)
        .put("host", p.host).put("sshPort", p.sshPort).put("user", p.user)
        // Ключ на телефоне — файл приложения, пути у человека нет: экран
        // показывает «ключ сохранён» и кнопку «Вставить ключ» (/api/key).
        .put("keyPath", "").put("keySaved", hasKey(s, p.id))
        .put("socksPort", 0).put("httpPort", 0)
        .put("poolSize", p.poolSize)
        .put("filterMode", p.filterMode).put("filterApps", JSONArray(p.filterApps.sorted()))
        .put("directHosts", lines(p.directHosts)).put("tunnelHosts", lines(p.tunnelHosts))
        .put("localViaTunnel", p.localViaTunnel)
        .put("panel", p.panel).put("deviceName", p.deviceName)
        .put("udpRelayEnabled", s.udpRelayEnabled)
        .put("meshEnabled", p.meshEnabled).put("meshKey", p.meshKey).put("meshName", p.meshName)
        .put("meshIncoming", p.meshIncoming).put("meshDirectOff", p.meshDirectOff)

    private fun config(s: Settings): JSONObject {
        val arr = JSONArray()
        for (p in s.profiles) arr.put(profileJson(s, p))
        return JSONObject()
            .put("profiles", arr)
            .put("activeProfile", s.activeProfileId)
            .put("sysProxy", false).put("setEnvVars", false).put("verbose", false)
            .put("autoStart", s.startMode == "on").put("autoConnect", s.startMode == "on")
            .put("startMode", s.startMode).put("lastRunning", s.lastRunning)
            .put("meshStartMode", s.meshStartMode).put("lastMesh", s.lastMesh)
            .put("theme", s.theme).put("background", s.background)
            .put("adBlockEnabled", s.adBlockEnabled)
            .put("adBlockSources", s.adBlockSources).put("adBlockAllowlist", s.adBlockAllowlist)
            .put("language", s.language)
            .put("showServerPicker", s.showServerPicker)
            .put("autoPickFastest", false)
    }

    private fun applyProfile(p: Settings.Profile, o: JSONObject) {
        if (o.has("name")) p.name = o.optString("name")
        if (o.has("flag")) p.flag = o.optString("flag")
        if (o.has("host")) p.host = o.optString("host").trim()
        if (o.optInt("sshPort") > 0) p.sshPort = o.optInt("sshPort")
        if (o.has("user")) p.user = o.optString("user").trim()
        if (o.optInt("poolSize") > 0) p.poolSize = o.optInt("poolSize")
        if (o.has("filterMode")) p.filterMode = o.optString("filterMode", "all").ifBlank { "all" }
        o.optJSONArray("filterApps")?.let { a ->
            p.filterApps = (0 until a.length()).map { a.optString(it).trim() }.filter { it.isNotEmpty() }.toMutableSet()
        }
        if (o.has("directHosts")) p.directHosts = joinLines(o.optJSONArray("directHosts"))
        if (o.has("tunnelHosts")) p.tunnelHosts = joinLines(o.optJSONArray("tunnelHosts"))
        if (o.has("localViaTunnel")) p.localViaTunnel = o.optBoolean("localViaTunnel")
        if (o.has("meshEnabled")) p.meshEnabled = o.optBoolean("meshEnabled")
        if (o.has("meshKey")) p.meshKey = o.optString("meshKey").trim()
        if (o.has("meshName")) p.meshName = o.optString("meshName").trim()
        if (o.has("meshIncoming")) p.meshIncoming = o.optBoolean("meshIncoming")
        if (o.has("meshDirectOff")) p.meshDirectOff = o.optBoolean("meshDirectOff")
    }

    private fun saveConfig(s: Settings, doc: JSONObject): String {
        val arr = doc.optJSONArray("profiles")
        if (arr != null) {
            val list = s.profiles
            for (i in 0 until arr.length()) {
                val o = arr.optJSONObject(i) ?: continue
                val p = list.find { it.id == o.optString("id") } ?: continue
                applyProfile(p, o)
                s.saveProfile(p)
                if (o.has("udpRelayEnabled")) s.udpRelayEnabled = o.optBoolean("udpRelayEnabled")
            }
        }
        if (doc.has("startMode")) s.startMode = doc.optString("startMode", "off")
        if (doc.has("meshStartMode")) s.meshStartMode = doc.optString("meshStartMode")
        if (doc.has("theme")) s.theme = doc.optString("theme")
        if (doc.has("background")) s.background = doc.optString("background")
        if (doc.has("language")) s.language = if (doc.optString("language") == "en") "en" else "ru"
        if (doc.has("showServerPicker")) s.showServerPicker = doc.optBoolean("showServerPicker")
        if (doc.has("adBlockEnabled")) s.adBlockEnabled = doc.optBoolean("adBlockEnabled")
        if (doc.has("adBlockSources")) s.adBlockSources = doc.optString("adBlockSources")
        if (doc.has("adBlockAllowlist")) s.adBlockAllowlist = doc.optString("adBlockAllowlist")
        pushState()
        val r = JSONObject().put("ok", true).put("config", config(s))
        if (running()) r.put("note", app.getString(R.string.applies_on_reconnect))
        return r.toString()
    }

    private fun saveKey(s: Settings, o: JSONObject): String {
        val key = o.optString("key").trim()
        if (!key.startsWith("-----BEGIN")) return err(app.getString(R.string.key_bad))
        val id = o.optString("id").ifBlank { s.activeProfileId }
        if (s.profiles.none { it.id == id }) return err("сервер не найден")
        s.saveKeyFor(id, key)
        return JSONObject().put("ok", true).put("config", config(s)).toString()
    }

    // ---------------------------------------------------------------------
    // Серверы
    // ---------------------------------------------------------------------

    /** Настройки сменились целиком — работающий туннель поднимается заново. */
    private fun restartIfRunning(s: Settings) {
        if (TunnelService.state == "stopped") return
        service(TunnelService.ACTION_STOP)
        // Служба останавливается не мгновенно: запуск — следующим сообщением.
        main.postDelayed({ if (Settings(app).ready()) launch(true, true) }, 900)
    }

    private fun selectProfile(s: Settings, id: String): String {
        if (s.profiles.none { it.id == id }) return err("сервер не найден")
        if (id == s.activeProfileId) return JSONObject().put("config", config(s)).toString()
        val wasRunning = TunnelService.state != "stopped"
        s.setActive(id)
        restartIfRunning(s)
        val r = JSONObject().put("config", config(s))
        r.put("note", app.getString(if (wasRunning) R.string.server_switched_reconnecting else R.string.server_switched))
        return r.toString()
    }

    private fun removeProfile(s: Settings, id: String): String {
        if (s.profiles.size <= 1) return err("последний сервер не удаляется")
        val wasActive = id == s.activeProfileId
        s.removeProfile(id)
        if (wasActive) restartIfRunning(s)
        return JSONObject().put("config", config(s)).toString()
    }

    private fun exportProfile(s: Settings, o: JSONObject): String {
        val p = s.profiles.find { it.id == o.optString("id") } ?: return err("сервер не найден")
        val withKey = o.optBoolean("includeKey", true) && hasKey(s, p.id)
        val key = if (withKey) s.keyFile(p.id).readText() else ""
        val data = mobile.Mobile.buildConfig(
            p.name, p.flag, p.host, p.sshPort.toLong(), p.user, p.poolSize.toLong(),
            p.filterMode, p.filterApps.joinToString("\n"), p.directHosts, p.localViaTunnel,
            withKey, key, p.panel, p.deviceName, p.meshKey,
        )
        return JSONObject().put("data", data).put("keyIncluded", withKey).toString()
    }

    /**
     * Сервер из текста (файл экспорта, ссылка sshtunnel:// или QR) — тот же
     * разбор ядром, что на ПК. Пустой текущий сервер (первый запуск)
     * заменяется новым, иначе новый добавляется рядом.
     */
    fun importText(s: Settings, text: String): String {
        val parsed = try {
            mobile.Mobile.parseConfig(text)
        } catch (e: Exception) {
            return err(e.message ?: app.getString(R.string.import_bad_apps))
        }
        val emptyActive = s.active().host.isBlank()
        val oldId = s.activeProfileId
        val p = s.addProfile(parsed.getName(), parsed.getFlag())
        p.host = parsed.getHost()
        p.sshPort = parsed.getSshPort().toInt().let { if (it > 0) it else 22 }
        p.user = parsed.getUser().ifBlank { "tunnel" }
        p.poolSize = parsed.getPoolSize().toInt().let { if (it > 0) it else 4 }
        p.filterMode = parsed.getFilterMode().ifBlank { "all" }
        p.filterApps = parsed.getFilterApps().split("\n").map { it.trim() }.filter { it.isNotBlank() }.toMutableSet()
        p.directHosts = parsed.getDirectHosts()
        p.localViaTunnel = parsed.getLocalViaTunnel()
        p.panel = parsed.getPanel()
        p.deviceName = parsed.getDeviceName()
        if (parsed.getMeshKey().isNotBlank()) {
            p.meshEnabled = true
            p.meshKey = parsed.getMeshKey()
        }
        s.saveProfile(p)
        val withKey = parsed.getKeyIncluded() && parsed.getKeyContents().isNotBlank()
        if (withKey) s.saveKeyFor(p.id, parsed.getKeyContents())
        if (emptyActive && TunnelService.state == "stopped") {
            s.setActive(p.id)
            if (oldId != p.id) s.removeProfile(oldId)
        }
        pushState()
        return JSONObject().put("ok", true).put("profile", JSONObject().put("id", p.id))
            .put("config", config(s)).put("keyImported", withKey).toString()
    }

    private fun latency(s: Settings): String {
        val list = s.profiles.filter { it.host.isNotBlank() }
        val results = arrayOfNulls<JSONObject>(list.size)
        val threads = list.mapIndexed { i, p ->
            Thread {
                val ms = mobile.Mobile.latency(p.host, p.sshPort.toLong())
                results[i] = JSONObject().put("id", p.id).put("ms", if (ms > 0) ms else 0)
                    .apply { if (ms < 0) put("error", "нет ответа") }
            }.apply { start() }
        }
        threads.forEach { it.join(10_000) }
        return JSONObject().put("results", JSONArray(results.filterNotNull())).toString()
    }

    // ---------------------------------------------------------------------
    // Профили приложения
    // ---------------------------------------------------------------------

    private fun appProfilesState(ap: AppProfiles): JSONObject {
        val arr = JSONArray()
        for (p in ap.list) arr.put(JSONObject().put("id", p.id))
        return JSONObject().put("enabled", ap.enabled).put("active", ap.active).put("profiles", arr)
    }

    private fun appProfileAction(s: Settings, path: String, o: JSONObject): String {
        val ap = AppProfiles(app, s)
        val wasRunning = TunnelService.state != "stopped"
        val what = path.substringAfterLast('/')
        if (what != "enable" && wasRunning) service(TunnelService.ACTION_STOP)
        try {
            when (what) {
                "enable" -> ap.setEnabled(o.optBoolean("on"))
                "add" -> ap.add()
                "switch" -> ap.switchTo(o.optString("id"))
                "delete" -> ap.delete(o.optString("id"))
            }
        } catch (e: Exception) {
            return JSONObject().put("error", app.getString(R.string.bundle_bad)).put("state", appProfilesState(ap)).toString()
        }
        if (what != "enable" && wasRunning) {
            main.postDelayed({ if (Settings(app).ready()) launch(true, true) }, 900)
        }
        return JSONObject().put("state", appProfilesState(ap)).toString()
    }

    // ---------------------------------------------------------------------
    // Приложения телефона (выбор, какие вести через туннель)
    // ---------------------------------------------------------------------

    private fun apps(): String {
        val pm = app.packageManager
        val list = pm.getInstalledApplications(0)
            .filter { pm.getLaunchIntentForPackage(it.packageName) != null && it.packageName != app.packageName }
            .map { it.packageName to (try { pm.getApplicationLabel(it).toString() } catch (_: Exception) { it.packageName }) }
            .sortedBy { it.second.lowercase() }
        val arr = JSONArray()
        for ((pkg, label) in list) arr.put(JSONObject().put("name", pkg).put("label", label).put("path", ""))
        return JSONObject().put("processes", arr).toString()
    }

    private fun appIcon(pkg: String): String {
        if (pkg.isBlank()) return ""
        return try {
            val d = app.packageManager.getApplicationIcon(pkg)
            val size = 96
            val bmp = Bitmap.createBitmap(size, size, Bitmap.Config.ARGB_8888)
            val c = Canvas(bmp)
            d.setBounds(0, 0, size, size)
            d.draw(c)
            val out = ByteArrayOutputStream()
            bmp.compress(Bitmap.CompressFormat.PNG, 100, out)
            "png:" + Base64.encodeToString(out.toByteArray(), Base64.NO_WRAP)
        } catch (_: Exception) {
            ""
        }
    }

    // ---------------------------------------------------------------------
    // Обновление
    // ---------------------------------------------------------------------

    @Volatile private var lastUpdate: UpdateCheck.Result? = null

    private fun updateCheck(): String {
        val r = try {
            UpdateCheck.check(BuildConfig.VERSION_NAME)
        } catch (e: UpdateCheck.RateLimited) {
            return JSONObject().put("error", "rate limited").put("code", "update_rate_limited").toString()
        } catch (e: Exception) {
            return err(e.message ?: "не удалось проверить")
        }
        lastUpdate = r
        return JSONObject().put("result", JSONObject()
            .put("installed", r.installed).put("latest", r.latest)
            .put("newer", r.newer).put("ahead", r.ahead).put("unknown", false)
            .put("url", r.releaseUrl).put("assetUrl", r.apkUrl)).toString()
    }

    /**
     * Скачивает APK сама программа (она исключена из VPN, идёт к GitHub
     * напрямую) и открывает системный установщик.
     */
    private fun updateDownload(): String {
        val u = lastUpdate ?: return err("сначала проверь обновления")
        if (u.apkUrl.isBlank()) return err("у релиза нет APK")
        val dir = java.io.File(app.cacheDir, "updates").apply { mkdirs() }
        dir.listFiles()?.forEach { it.delete() }
        val file = java.io.File(dir, "ssh_tunnel-${u.latest}.apk")
        val part = java.io.File(dir, file.name + ".part")
        try {
            val conn = java.net.URL(u.apkUrl).openConnection() as java.net.HttpURLConnection
            conn.connectTimeout = 15000
            conn.readTimeout = 30000
            conn.instanceFollowRedirects = true
            if (conn.responseCode != 200) throw Exception("HTTP ${conn.responseCode}")
            val total = conn.contentLengthLong
            var done = 0L
            conn.inputStream.use { inp -> part.outputStream().use { out -> done = inp.copyTo(out, 64 * 1024) } }
            if (total > 0 && done != total) throw Exception("оборвалось на $done из $total")
            if (!part.renameTo(file)) throw Exception("не удалось сохранить файл")
        } catch (e: Exception) {
            part.delete()
            return err(e.message ?: "не скачалось")
        }
        main.post { installApk(file) }
        return JSONObject().put("ok", true).put("path", file.name).toString()
    }

    private fun installApk(file: java.io.File) {
        try {
            val uri = androidx.core.content.FileProvider.getUriForFile(app, "${app.packageName}.files", file)
            app.startActivity(
                Intent(Intent.ACTION_VIEW)
                    .setDataAndType(uri, "application/vnd.android.package-archive")
                    .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
            )
        } catch (_: Exception) {
        }
    }

    // ---------------------------------------------------------------------

    private fun obj(body: String): JSONObject = try { JSONObject(body) } catch (_: Exception) { JSONObject() }

    private fun err(msg: String): String = JSONObject().put("error", msg).toString()
}
