package io.github.vitazgio.sshtunnel

import android.app.Activity
import android.app.PendingIntent
import android.app.assist.AssistStructure
import android.content.Intent
import android.os.Bundle
import android.os.CancellationSignal
import android.service.autofill.AutofillService
import android.service.autofill.Dataset
import android.service.autofill.FillCallback
import android.service.autofill.FillRequest
import android.service.autofill.FillResponse
import android.service.autofill.SaveCallback
import android.service.autofill.SaveInfo
import android.service.autofill.SaveRequest
import android.text.InputType
import android.view.View
import android.view.autofill.AutofillId
import android.view.autofill.AutofillManager
import android.view.autofill.AutofillValue
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.RemoteViews
import android.widget.TextView
import org.json.JSONArray

/**
 * «Пароли» → автозаполнение Android: в поле логина и пароля любого
 * приложения (и браузера, где он отдаёт поля службе) — подсказка со своими
 * логинами для этого сайта/приложения. Хранилище закрыто — подсказка
 * «Открыть ssh_tunnel»: VaultUnlockActivity спросит главный пароль.
 * Человек включает службу сам: Настройки → Пароли и автозаполнение.
 */
class VaultFill : AutofillService() {
    /** Поля формы: логин и пароль, и чей экран (сайт или пакет). */
    class Form(var user: AutofillId? = null, var pass: AutofillId? = null, var web: String = "", var pkg: String = "")

    companion object {
        fun parse(s: AssistStructure): Form {
            val f = Form(pkg = s.activityComponent?.packageName ?: "")
            for (i in 0 until s.windowNodeCount) walk(s.getWindowNodeAt(i).rootViewNode, f)
            return f
        }

        private fun walk(n: AssistStructure.ViewNode, f: Form) {
            n.webDomain?.takeIf { it.isNotEmpty() }?.let { f.web = it }
            val hints = (n.autofillHints?.joinToString(" ") ?: "").lowercase() + " " +
                (n.idEntry ?: "").lowercase() + " " + (n.hint ?: "").lowercase()
            val type = n.inputType
            val isPass = hints.contains("password") || hints.contains("парол") ||
                (type and InputType.TYPE_TEXT_VARIATION_PASSWORD) == InputType.TYPE_TEXT_VARIATION_PASSWORD ||
                (type and InputType.TYPE_TEXT_VARIATION_WEB_PASSWORD) == InputType.TYPE_TEXT_VARIATION_WEB_PASSWORD
            val isUser = !isPass && (hints.contains("username") || hints.contains("email") || hints.contains("login") ||
                hints.contains("логин") || hints.contains("почт") || hints.contains("phone") ||
                (type and InputType.TYPE_TEXT_VARIATION_EMAIL_ADDRESS) == InputType.TYPE_TEXT_VARIATION_EMAIL_ADDRESS)
            val id = n.autofillId
            if (id != null && n.autofillType == View.AUTOFILL_TYPE_TEXT) {
                if (isPass && f.pass == null) f.pass = id
                else if (isUser && f.user == null) f.user = id
            }
            for (i in 0 until n.childCount) walk(n.getChildAt(i), f)
        }

        fun site(f: Form) = f.web.ifEmpty { f.pkg }

        private fun row(pkg: String, text: String) = RemoteViews(pkg, android.R.layout.simple_list_item_1).apply {
            setTextViewText(android.R.id.text1, text)
        }

        /** Подсказки с логинами (хранилище открыто). */
        fun datasets(pkg: String, f: Form, b: FillResponse.Builder): Int {
            val arr = JSONArray(mobile.Mobile.vaultMatchJSON(site(f)))
            for (i in 0 until arr.length()) {
                val o = arr.getJSONObject(i)
                val label = "🔑 " + (o.optString("login").ifEmpty { o.optString("title") })
                val d = Dataset.Builder(row(pkg, label))
                f.user?.let { d.setValue(it, AutofillValue.forText(o.optString("login"))) }
                f.pass?.let { d.setValue(it, AutofillValue.forText(o.optString("pass"))) }
                b.addDataset(d.build())
            }
            return arr.length()
        }

        fun saveInfo(f: Form): SaveInfo? {
            val ids = listOfNotNull(f.user, f.pass)
            if (f.pass == null) return null
            return SaveInfo.Builder(SaveInfo.SAVE_DATA_TYPE_PASSWORD or SaveInfo.SAVE_DATA_TYPE_USERNAME, ids.toTypedArray()).build()
        }
    }

    override fun onFillRequest(req: FillRequest, cancel: CancellationSignal, cb: FillCallback) {
        val s = req.fillContexts.lastOrNull()?.structure ?: return cb.onSuccess(null)
        val f = parse(s)
        if (f.pass == null && f.user == null) return cb.onSuccess(null)
        if (f.pkg == packageName) return cb.onSuccess(null)
        AndroidApi.init(this)
        val b = FillResponse.Builder()
        if (mobile.Mobile.vaultUnlocked()) {
            if (datasets(packageName, f, b) == 0 && saveInfo(f) == null) return cb.onSuccess(null)
        } else {
            // Закрыто: одна подсказка — открыть; после главного пароля
            // VaultUnlockActivity вернёт подсказки с логинами.
            val ids = listOfNotNull(f.user, f.pass).toTypedArray()
            val i = Intent(this, VaultUnlockActivity::class.java)
            val pi = PendingIntent.getActivity(this, 1, i, PendingIntent.FLAG_CANCEL_CURRENT or PendingIntent.FLAG_MUTABLE)
            b.setAuthentication(ids, pi.intentSender, row(packageName, getString(R.string.vault_locked)))
        }
        saveInfo(f)?.let { b.setSaveInfo(it) }
        cb.onSuccess(b.build())
    }

    override fun onSaveRequest(req: SaveRequest, cb: SaveCallback) {
        val s = req.fillContexts.lastOrNull()?.structure ?: return cb.onSuccess()
        val f = parse(s)
        var login = ""
        var pass = ""
        fun read(n: AssistStructure.ViewNode) {
            val v = n.autofillValue
            if (v != null && v.isText) {
                if (n.autofillId == f.user) login = v.textValue.toString()
                if (n.autofillId == f.pass) pass = v.textValue.toString()
            }
            for (i in 0 until n.childCount) read(n.getChildAt(i))
        }
        for (i in 0 until s.windowNodeCount) read(s.getWindowNodeAt(i).rootViewNode)
        if (pass.isEmpty()) return cb.onSuccess()
        AndroidApi.init(this)
        val err = mobile.Mobile.vaultSave(site(f), login, pass)
        if (err.isEmpty()) cb.onSuccess() else cb.onFailure(err)
    }
}

/** Главный пароль для автозаполнения: открыть хранилище и вернуть логины. */
class VaultUnlockActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        AndroidApi.init(this)
        val pad = (16 * resources.displayMetrics.density).toInt()
        val box = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(pad, pad, pad, pad) }
        val title = TextView(this).apply { setText(R.string.vault_unlock_title); textSize = 18f }
        val note = TextView(this)
        val input = EditText(this).apply {
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
            setHint(R.string.vault_master)
        }
        val go = Button(this).apply { setText(R.string.vault_unlock) }
        box.addView(title); box.addView(input); box.addView(go); box.addView(note)
        setContentView(box)
        go.setOnClickListener {
            val err = mobile.Mobile.vaultUnlock(input.text.toString())
            if (err.isNotEmpty()) { note.text = err; return@setOnClickListener }
            @Suppress("DEPRECATION")
            val s = intent.getParcelableExtra<AssistStructure>(AutofillManager.EXTRA_ASSIST_STRUCTURE)
            val reply = Intent()
            if (s != null) {
                val f = VaultFill.parse(s)
                val b = FillResponse.Builder()
                VaultFill.datasets(packageName, f, b)
                VaultFill.saveInfo(f)?.let { b.setSaveInfo(it) }
                reply.putExtra(AutofillManager.EXTRA_AUTHENTICATION_RESULT, b.build())
            }
            setResult(RESULT_OK, reply)
            finish()
        }
    }
}
