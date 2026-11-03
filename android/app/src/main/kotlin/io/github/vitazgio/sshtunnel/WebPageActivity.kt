package io.github.vitazgio.sshtunnel

import android.annotation.SuppressLint
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.net.Uri
import android.os.Bundle
import android.webkit.JavascriptInterface
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.activity.OnBackPressedCallback
import androidx.appcompat.app.AppCompatActivity
import org.json.JSONArray

// Общая страница интерфейса — та же, что в окне ПК (Книга, дополнительные
// приложения): HTML и API отдаёт ядро (mobile.StartUI), здесь только WebView
// и мост SSHTunnelAndroid. Новая кнопка на странице появляется на телефоне
// без кода под Android.
class WebPageActivity : AppCompatActivity() {

    private lateinit var web: WebView
    private var base = ""

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Look.apply(this)
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
    }

    companion object {
        const val EXTRA_URL = "url"
        const val EXTRA_PAGE = "page"
        const val EXTRA_OPEN = "open"
        const val EXTRA_CHAPTER = "chapter"
        const val PAGE_APPS = "apps.html"
        const val PAGE_BOOK = "book.html"

        fun open(ctx: Context, page: String, app: String? = null) {
            ctx.startActivity(Intent(ctx, WebPageActivity::class.java)
                .putExtra(EXTRA_PAGE, page)
                .apply { if (app != null) putExtra(EXTRA_OPEN, app) })
        }

        // starred — приложения со звёздочкой (id → название) для быстрого
        // доступа на главном экране.
        fun starred(ctx: Context, lang: String): List<Pair<String, String>> = try {
            mobile.Mobile.startUI(ctx.filesDir.absolutePath)
            val a = JSONArray(mobile.Mobile.starredJSON(lang))
            (0 until a.length()).map { a.getJSONObject(it).let { o -> o.getString("id") to o.getString("name") } }
        } catch (_: Exception) {
            emptyList()
        }
    }
}
