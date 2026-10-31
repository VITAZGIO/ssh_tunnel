package io.github.vitazgio.sshtunnel

import android.os.Bundle
import android.view.View
import android.view.ViewGroup
import android.widget.CheckBox
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.PopupWindow
import android.widget.TextView
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import mobile.Mobile
import org.json.JSONObject

/**
 * Блокировка рекламы — отдельным экраном, устроенным как «Исключения
 * доменов»: две вкладки над одним полем — чёрный список (откуда брать списки
 * блокировки) и белый (что не трогать, даже если совпало с чёрным), ниже
 * перенос обоих списков буфером или файлом и одна кнопка «Сохранить». Она же
 * и скачивает списки по ссылкам: отдельная «Обновить список» была лишней.
 */
class AdBlockActivity : AppCompatActivity() {

    private lateinit var settings: Settings

    private lateinit var enabledCheck: CheckBox
    private lateinit var tileBlocked: TextView
    private lateinit var tabs: LinearLayout
    private lateinit var tabLabel: TextView
    private lateinit var listEdit: EditText
    private lateinit var saveButton: View
    private lateinit var statusView: TextView

    /** true — редактируется чёрный список (adBlockSources), false — белый (adBlockAllowlist). */
    private var onBlockTab = true

    // Текст обеих вкладок, пока экран открыт: в настройки он попадает по
    // «Сохранить» (или при уходе с экрана).
    private var sources = ""
    private var allowlist = ""

    private val saveFile = registerForActivityResult(
        ActivityResultContracts.CreateDocument("application/json")
    ) { uri ->
        if (uri != null) {
            statusView.setText(if (Xfer.write(this, uri, exportText())) R.string.saved_to_file else R.string.file_failed)
        }
    }

    private val openFile = registerForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null) importText(Xfer.read(this, uri))
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Look.apply(this)
        setContentView(R.layout.activity_adblock)
        Look.backdrop(this)
        settings = Settings(this)

        androidx.core.view.ViewCompat.setOnApplyWindowInsetsListener(
            findViewById(R.id.root)
        ) { view, insets ->
            val bars = insets.getInsets(
                androidx.core.view.WindowInsetsCompat.Type.systemBars() or
                    androidx.core.view.WindowInsetsCompat.Type.displayCutout()
            )
            view.setPadding(bars.left + view.paddingLeft, bars.top, bars.right + view.paddingRight, bars.bottom)
            insets
        }

        findViewById<View>(R.id.backFromAdBlock).setOnClickListener { storeLists(); finish() }

        enabledCheck = findViewById(R.id.adBlockEnabled)
        tileBlocked = findViewById(R.id.tileBlocked)
        tabs = findViewById(R.id.abTabs)
        tabLabel = findViewById(R.id.tabLabel)
        listEdit = findViewById(R.id.listEdit)
        saveButton = findViewById(R.id.saveAdBlock)
        statusView = findViewById(R.id.adBlockStatus)

        sources = settings.adBlockSources
        allowlist = settings.adBlockAllowlist
        enabledCheck.isChecked = settings.adBlockEnabled
        enabledCheck.setOnCheckedChangeListener { _, checked -> settings.adBlockEnabled = checked }

        tileBlocked.text = getString(R.string.tile_blocked) + ": " + settings.adBlockTotal

        for (k in 0 until tabs.childCount) {
            val c = tabs.getChildAt(k)
            c.setOnClickListener { switchTab(c.tag == "block") }
        }
        // Поле сразу с чёрным списком: switchTab сперва забирает текст поля
        // в открытую вкладку, и пустое поле затёрло бы загруженный список.
        listEdit.setText(sources)
        switchTab(true)

        findViewById<View>(R.id.tabInfo).setOnClickListener {
            showTip(it, getString(if (onBlockTab) R.string.ad_block_sources_note else R.string.ad_block_allowlist_hint))
        }

        Xfer.bindModeSeg(findViewById(R.id.xferModeSeg), settings)
        findViewById<View>(R.id.abExportBtn).setOnClickListener { onExport() }
        findViewById<View>(R.id.abImportBtn).setOnClickListener { onImport() }
        saveButton.setOnClickListener { onSave() }
    }

    override fun onPause() {
        super.onPause()
        storeLists()
    }

    private fun switchTab(block: Boolean) {
        takeField()
        onBlockTab = block
        for (k in 0 until tabs.childCount) {
            val c = tabs.getChildAt(k)
            c.setBackgroundResource(if ((c.tag == "block") == block) R.drawable.bg_seg_on else 0)
        }
        tabLabel.setText(if (block) R.string.ad_block_sources else R.string.ad_block_allowlist)
        listEdit.hint = getString(if (block) R.string.ad_block_sources_hint else R.string.ad_block_allowlist_hint)
        listEdit.setText(if (block) sources else allowlist)
    }

    /** Переносит текст поля в список открытой вкладки. */
    private fun takeField() {
        if (!::listEdit.isInitialized) return
        val text = listEdit.text.toString()
        if (onBlockTab) sources = text else allowlist = text
    }

    private fun storeLists() {
        takeField()
        settings.adBlockSources = sources.trim()
        settings.adBlockAllowlist = allowlist.trim()
    }

    /**
     * «Сохранить»: списки — в настройки, и сразу скачать блокировку по
     * ссылкам из чёрного списка. Сеть небыстрая, поэтому в отдельном потоке;
     * пока идёт загрузка, кнопка недоступна. Не скачалось — экран остаётся
     * открытым с причиной, сами списки при этом уже сохранены.
     */
    private fun onSave() {
        storeLists()
        val src = settings.adBlockSources
        if (src.isBlank()) {
            Toast.makeText(this, R.string.saved, Toast.LENGTH_SHORT).show()
            finish()
            return
        }
        saveButton.isEnabled = false
        statusView.setText(R.string.ad_block_updating)
        Thread {
            val result = try {
                Mobile.updateBlockLists(src, settings.adBlockListFile.absolutePath)
            } catch (e: Exception) {
                JSONObject().put("error", e.message ?: "").toString()
            }
            runOnUiThread {
                saveButton.isEnabled = true
                val o = try { JSONObject(result) } catch (e: Exception) { JSONObject() }
                val err = o.optString("error")
                if (err.isNotBlank()) {
                    statusView.text = getString(R.string.ad_block_update_failed, err)
                } else {
                    Toast.makeText(
                        this, getString(R.string.saved) + " · " + getString(R.string.ad_block_updated, o.optInt("count")),
                        Toast.LENGTH_LONG
                    ).show()
                    finish()
                }
            }
        }.start()
    }

    /** {"sshTunnelAdBlock":1,"sources":"…","allowlist":"…"} — оба списка разом. */
    private fun exportText(): String {
        takeField()
        return JSONObject()
            .put("sshTunnelAdBlock", 1)
            .put("sources", sources.trim())
            .put("allowlist", allowlist.trim())
            .toString(2)
    }

    private fun onExport() {
        if (Xfer.toFile(settings)) {
            saveFile.launch("ssh_tunnel-adblock.json")
            return
        }
        Xfer.copy(this, getString(R.string.ad_block_title), exportText())
        statusView.setText(R.string.copied)
    }

    private fun onImport() {
        if (Xfer.toFile(settings)) {
            openFile.launch(arrayOf("application/json", "text/plain", "*/*"))
            return
        }
        importText(Xfer.paste(this))
    }

    /** Свой JSON — в обе вкладки; просто текст — в ту, что открыта. */
    private fun importText(text: String) {
        if (text.isBlank()) {
            statusView.setText(R.string.paste_failed)
            return
        }
        val o = try { JSONObject(text.trim()) } catch (e: Exception) { null }
        if (o != null && o.has("sshTunnelAdBlock")) {
            sources = o.optString("sources")
            allowlist = o.optString("allowlist")
            listEdit.setText(if (onBlockTab) sources else allowlist)
        } else {
            listEdit.setText(text.trim())
            takeField()
        }
        statusView.setText(R.string.ad_block_imported)
    }

    private fun showTip(anchor: View, text: String) {
        val tv = TextView(this)
        tv.text = text
        tv.setTextColor(tc(R.color.text))
        tv.textSize = 12.5f
        tv.setPadding(dp(12), dp(10), dp(12), dp(10))
        val popup = PopupWindow(tv, dp(240), ViewGroup.LayoutParams.WRAP_CONTENT, true)
        popup.setBackgroundDrawable(ContextCompat.getDrawable(this, R.drawable.bg_card))
        popup.elevation = 12f
        popup.showAsDropDown(anchor, 0, 4)
    }

    private fun dp(v: Int): Int = (v * resources.displayMetrics.density).toInt()
}
