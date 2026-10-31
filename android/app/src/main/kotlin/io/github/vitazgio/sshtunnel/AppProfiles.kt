package io.github.vitazgio.sshtunnel

import android.content.Context
import org.json.JSONArray
import org.json.JSONObject
import java.io.File

/**
 * Профили приложения — несколько полных наборов настроек (серверы с ключами,
 * приложения, галочки), между которыми можно быстро переключаться. Как на
 * компьютере (src/internal/appprofiles): действующий профиль — это обычные
 * настройки приложения, остальные лежат снимками в файлах.
 *
 * Снимок — та же выгрузка, что у «Переноса настроек» ([SettingsBundle]):
 * она и так умеет сохранить и вернуть всё целиком.
 */
class AppProfiles(context: Context, private val settings: Settings) {

    data class Profile(val id: String)

    private val dir = File(context.filesDir, "appprofiles")
    private val index = File(dir, "index.json")

    var enabled = false
        private set
    var active = "p1"
        private set
    val list = mutableListOf<Profile>()

    init {
        load()
    }

    private fun load() {
        list.clear()
        try {
            val o = JSONObject(index.readText())
            enabled = o.optBoolean("enabled")
            active = o.optString("active")
            val arr = o.optJSONArray("profiles") ?: JSONArray()
            for (i in 0 until arr.length()) list.add(Profile(arr.getJSONObject(i).getString("id")))
        } catch (_: Exception) {
        }
        if (list.isEmpty()) list.add(Profile("p1"))
        if (list.none { it.id == active }) active = list.first().id
    }

    private fun save() {
        dir.mkdirs()
        val arr = JSONArray()
        for (p in list) arr.put(JSONObject().put("id", p.id))
        index.writeText(JSONObject().put("enabled", enabled).put("active", active).put("profiles", arr).toString())
    }

    private fun snap(id: String) = File(dir, "$id.json")

    fun setEnabled(on: Boolean) {
        enabled = on
        save()
    }

    /** Новый профиль с нуля; текущие настройки уходят в снимок прежнего. */
    fun add() {
        dir.mkdirs()
        snap(active).writeText(SettingsBundle.build(settings))
        var n = list.size + 1
        while (list.any { it.id == "p$n" }) n++
        val id = "p$n"
        snap(id).delete()
        list.add(Profile(id))
        active = id
        save()
        reset()
    }

    /** Переключиться на профиль [id]. */
    fun switchTo(id: String) {
        if (id == active || list.none { it.id == id }) return
        dir.mkdirs()
        snap(active).writeText(SettingsBundle.build(settings))
        restore(id)
        active = id
        save()
    }

    /** Удалить профиль. Последний не удаляется; удалили выбранный — соседний. */
    fun delete(id: String) {
        val i = list.indexOfFirst { it.id == id }
        if (i < 0 || list.size == 1) return
        if (id == active) {
            val next = list[if (i > 0) i - 1 else 1].id
            restore(next)
            active = next
        }
        list.removeAt(i)
        snap(id).delete()
        save()
    }

    private fun restore(id: String) {
        val f = snap(id)
        if (f.exists()) SettingsBundle.apply(settings, f.readText()) else reset()
    }

    /**
     * Настройки с нуля: один пустой сервер, всё по умолчанию. Язык и
     * оформление остаются — менять их ради нового профиля никто не хочет.
     */
    private fun reset() {
        val blank = Settings.Profile(
            id = "", name = "", flag = "", host = "", sshPort = 22, user = "tunnel", poolSize = 4,
            directHosts = "", localViaTunnel = false, filterMode = "all", filterApps = mutableSetOf(),
            panel = "", deviceName = "", meshEnabled = false, meshKey = "", tunnelHosts = "",
        )
        settings.replaceProfiles(listOf(blank to ""), 0)
        settings.startMode = "off"
        settings.meshStartMode = ""
        settings.adBlockEnabled = false
        settings.adBlockSources = ""
        settings.adBlockAllowlist = ""
        settings.udpRelayEnabled = false
        settings.showServerPicker = false
    }
}
