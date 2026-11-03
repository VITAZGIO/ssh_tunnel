package io.github.vitazgio.sshtunnel

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Color
import android.net.Uri
import android.os.Bundle
import android.webkit.JavascriptInterface
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.activity.OnBackPressedCallback
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity

// Общая страница интерфейса — та же, что в окне ПК (Книга, дополнительные
// приложения): HTML и API отдаёт ядро (mobile.StartUI), здесь только WebView
// и мост SSHTunnelAndroid. Новая кнопка на странице появляется на телефоне
// без кода под Android.
class WebPageActivity : AppCompatActivity() {

    private lateinit var web: WebView
    private var base = ""
    private var fileCallback: ValueCallback<Array<Uri>>? = null
    private val pickFiles = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { r ->
        fileCallback?.onReceiveValue(WebChromeClient.FileChooserParams.parseResult(r.resultCode, r.data))
        fileCallback = null
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Look.apply(this)
        DeviceApps.init(this)
        base = try {
            mobile.Mobile.startUI(filesDir.absolutePath)
        } catch (e: Exception) {
            finish()
            return
        }
        web = WebView(this)
        web.setBackgroundColor(Color.TRANSPARENT)
        // Фон рисует сама страница (theme.css) — тот же, что на ПК.
        setContentView(web)
        web.settings.javaScriptEnabled = true
        web.settings.domStorageEnabled = true
        web.settings.allowFileAccess = false
        web.addJavascriptInterface(Bridge(), "SSHTunnelAndroid")
        web.webChromeClient = object : WebChromeClient() {
            // «Загрузить сюда» в Файлах устройства: системный выбор файлов.
            override fun onShowFileChooser(
                view: WebView, cb: ValueCallback<Array<Uri>>, params: FileChooserParams,
            ): Boolean {
                fileCallback?.onReceiveValue(null)
                fileCallback = cb
                try {
                    pickFiles.launch(params.createIntent())
                } catch (_: Exception) {
                    fileCallback = null
                    return false
                }
                return true
            }
        }
        web.webViewClient = object : WebViewClient() {
            // Свои страницы — здесь, всё остальное — в браузере телефона.
            override fun shouldOverrideUrlLoading(view: WebView, req: WebResourceRequest): Boolean {
                val u = req.url.toString()
                if (u.startsWith("$base/")) return false
                openExternal(u)
                return true
            }
        }
        onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
            override fun handleOnBackPressed() {
                if (web.canGoBack()) web.goBack() else finish()
            }
        })
        // Готовый адрес страницы от главного экрана (Книга, приложение
        // окном): путь ядра с параметрами.
        intent.getStringExtra(EXTRA_URL)?.takeIf { it.startsWith("/") }?.let {
            web.loadUrl(base + it)
            return
        }
        val s = Settings(this)
        val q = Uri.Builder()
            .appendQueryParameter("t", mobile.Mobile.uiToken())
            .appendQueryParameter("lang", if (s.language == "en") "en" else "ru")
        if (s.theme.isNotEmpty()) q.appendQueryParameter("theme", s.theme)
        intent.getStringExtra(EXTRA_OPEN)?.let { q.appendQueryParameter("open", it) }
        intent.getStringExtra(EXTRA_CHAPTER)?.let { q.appendQueryParameter("ch", it) }
        val page = intent.getStringExtra(EXTRA_PAGE) ?: PAGE_APPS
        web.loadUrl("$base/$page" + q.build().toString())
    }

    override fun onDestroy() {
        if (::web.isInitialized) web.destroy()
        super.onDestroy()
    }

    private fun openExternal(url: String) {
        if (!url.startsWith("https://")) return
        try {
            startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)))
        } catch (_: Exception) {
        }
    }

    // Мост для страницы: только то, чего WebView не умеет сам.
    inner class Bridge {
        @JavascriptInterface
        fun close() = runOnUiThread { finish() }

        @JavascriptInterface
        fun openExternal(url: String) = runOnUiThread { this@WebPageActivity.openExternal(url) }

        // Календарь подключён на странице: спросить разрешения и завести
        // аккаунт в системе (CalendarSync).
        @JavascriptInterface
        fun calendarSetup(contacts: Boolean) = runOnUiThread { this@WebPageActivity.calendarSetup(contacts) }

        @JavascriptInterface
        fun calendarForget() = runOnUiThread { CalendarSync.forget(this@WebPageActivity) }

        @JavascriptInterface
        fun hasAllFiles(): Boolean = DeviceApps.hasAllFiles()

        @JavascriptInterface
        fun requestAllFiles() = runOnUiThread { DeviceApps.requestAllFiles(this@WebPageActivity) }
    }

    private var calendarContacts = false

    private fun calendarSetup(contacts: Boolean) {
        calendarContacts = contacts
        val perms = mutableListOf(Manifest.permission.READ_CALENDAR, Manifest.permission.WRITE_CALENDAR)
        if (contacts) perms += listOf(Manifest.permission.READ_CONTACTS, Manifest.permission.WRITE_CONTACTS)
        val need = perms.filter { checkSelfPermission(it) != PackageManager.PERMISSION_GRANTED }
        if (need.isEmpty()) CalendarSync.setup(this, contacts)
        else requestPermissions(need.toTypedArray(), REQ_CALENDAR)
    }

    override fun onRequestPermissionsResult(code: Int, perms: Array<out String>, res: IntArray) {
        super.onRequestPermissionsResult(code, perms, res)
        // Без разрешения на контакты календарь всё равно работает.
        if (code == REQ_CALENDAR) CalendarSync.setup(this,
            calendarContacts && checkSelfPermission(Manifest.permission.WRITE_CONTACTS) == PackageManager.PERMISSION_GRANTED)
    }

    companion object {
        const val EXTRA_URL = "url"
        const val EXTRA_PAGE = "page"
        const val EXTRA_OPEN = "open"
        const val EXTRA_CHAPTER = "chapter"
        const val PAGE_APPS = "apps.html"
        const val PAGE_BOOK = "book.html"
        private const val REQ_CALENDAR = 71

        fun open(ctx: Context, page: String, app: String? = null) {
            ctx.startActivity(Intent(ctx, WebPageActivity::class.java)
                .putExtra(EXTRA_PAGE, page)
                .apply { if (app != null) putExtra(EXTRA_OPEN, app) })
        }
    }
}
