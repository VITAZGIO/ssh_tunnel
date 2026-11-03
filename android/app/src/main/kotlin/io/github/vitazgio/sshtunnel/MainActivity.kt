package io.github.vitazgio.sshtunnel

import android.Manifest
import android.annotation.SuppressLint
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Color
import android.net.Uri
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.webkit.JavascriptInterface
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.TextView
import android.widget.Toast
import androidx.activity.OnBackPressedCallback
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import java.lang.ref.WeakReference

/**
 * Главный экран телефона — тот же index.html, что в окне ПК (общий HTML из
 * internal/uiassets, его отдаёт ядро). Данные экран берёт из того же API
 * (AndroidApi), здесь только WebView и мост SSHTunnelAndroid к тому, что
 * умеет лишь сам телефон: разрешение на VPN, камера для QR, буфер обмена,
 * файлы, ссылки наружу. Нативными остаются VPN-служба и плитка в шторке.
 */
class MainActivity : AppCompatActivity(), AndroidApi.Screen {

    private lateinit var web: WebView
    private var base = ""

    private val vpnPermission = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) { result -> AndroidApi.vpnAllowed(result.resultCode == RESULT_OK) }

    private val notificationPermission = registerForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { /* отказ не мешает работе, просто не будет уведомления */ }

    private val cameraPermission = registerForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { granted ->
        if (granted) launchScanner()
        else Toast.makeText(this, R.string.camera_denied, Toast.LENGTH_LONG).show()
    }

    private val scanLauncher = registerForActivityResult(ScanContract()) { result ->
        result.contents?.let { importText(it) }
    }

    // Сохранение файла (перенос настроек, списки) — системный выбор места.
    private var pendingSave = ""
    private val saveFile = registerForActivityResult(
        ActivityResultContracts.CreateDocument("application/json")
    ) { uri ->
        val text = pendingSave
        pendingSave = ""
        if (uri != null && text.isNotEmpty()) {
            val ok = try {
                contentResolver.openOutputStream(uri)?.use { it.write(text.toByteArray()) } != null
            } catch (_: Exception) {
                false
            }
            if (!ok) Toast.makeText(this, R.string.file_failed, Toast.LENGTH_LONG).show()
        }
    }

    // Открытие файла (<input type=file> на странице).
    private var fileCallback: ValueCallback<Array<Uri>>? = null
    private val openFile = registerForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        fileCallback?.onReceiveValue(if (uri != null) arrayOf(uri) else null)
        fileCallback = null
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Look.apply(this)
        if (!AndroidApi.init(this)) {
            // Без ядра экрана нет. VPN по-прежнему включается плиткой в шторке.
            setContentView(TextView(this).apply {
                setText(R.string.ui_failed)
                setPadding(48, 160, 48, 48)
            })
            return
        }
        base = mobile.Mobile.startUI(filesDir.absolutePath)

        web = WebView(this)
        web.setBackgroundColor(Color.TRANSPARENT)
        setContentView(web)
        // С Android 15 окно занимает весь экран — отступы под системные полосы.
        ViewCompat.setOnApplyWindowInsetsListener(web) { v, insets ->
            val bars = insets.getInsets(
                WindowInsetsCompat.Type.systemBars() or WindowInsetsCompat.Type.displayCutout() or
                    WindowInsetsCompat.Type.ime()
            )
            v.setPadding(bars.left, bars.top, bars.right, bars.bottom)
            insets
        }
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
        web.webChromeClient = object : WebChromeClient() {
            override fun onShowFileChooser(
                view: WebView, callback: ValueCallback<Array<Uri>>, params: FileChooserParams,
            ): Boolean {
                fileCallback?.onReceiveValue(null)
                fileCallback = callback
                return try {
                    openFile.launch(arrayOf("application/json", "text/plain", "*/*"))
                    true
                } catch (_: Exception) {
                    fileCallback = null
                    false
                }
            }
        }
        onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
            override fun handleOnBackPressed() {
                web.evaluateJavascript("window.androidBack ? androidBack() : false") { r ->
                    if (r != "true") moveTaskToBack(true)
                }
            }
        })
        web.loadUrl(AndroidApi.mainUrl())

        askNotificationPermission()
        if (savedInstanceState == null) {
            AndroidApi.applyStartMode()
            // Пересоздание экрана (поворот, тема) ссылку второй раз не импортирует.
            handleImportLink(intent)
        }
    }

    override fun onResume() {
        super.onResume()
        AndroidApi.screen = WeakReference(this)
        AndroidApi.pushState()
    }

    // Окно в фокусе — можно прочитать буфер: скопированное в другом приложении
    // уходит на компьютеры само (плагин «Общий буфер», Tools5.autoSend).
    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (hasFocus) Tools5.autoSend(this)
    }

    override fun onPause() {
        if (AndroidApi.screen?.get() === this) AndroidApi.screen = null
        super.onPause()
    }

    override fun onDestroy() {
        if (::web.isInitialized) web.destroy()
        super.onDestroy()
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        handleImportLink(intent)
    }

    /** Системный вопрос «разрешить VPN?» (AndroidApi, кнопка на экране). */
    override fun askVpnPermission() {
        val i = VpnService.prepare(this)
        if (i == null) AndroidApi.vpnAllowed(true) else vpnPermission.launch(i)
    }

    /**
     * Ссылка sshtunnel://import?… — её несёт QR, который панель сервера
     * показывает при создании клиента: системная камера или браузер
     * открывают её здесь.
     */
    private fun handleImportLink(intent: Intent?) {
        if (intent == null) return
        val uri = intent.data ?: return
        if (!uri.scheme.equals("sshtunnel", ignoreCase = true)) return
        intent.data = null
        importText(uri.toString())
    }

    /** Сервер из текста — тем же API, что «Вставить»; ответ — странице. */
    private fun importText(text: String) {
        Thread {
            val r = AndroidApi.importText(Settings(this), text)
            runOnUiThread {
                if (::web.isInitialized) web.evaluateJavascript("window.androidImported && androidImported($r)", null)
            }
        }.start()
    }

    private fun askNotificationPermission() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
        val granted = checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) ==
            PackageManager.PERMISSION_GRANTED
        if (!granted) notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
    }

    private fun launchScanner() {
        scanLauncher.launch(
            ScanOptions()
                .setOrientationLocked(true)
                .setBeepEnabled(false)
                .setDesiredBarcodeFormats(ScanOptions.QR_CODE)
                // Подсказка zxing не учитывает системные отступы — без неё.
                .setPrompt("")
        )
    }

    private fun openExternal(url: String) {
        if (!url.startsWith("https://") && !url.startsWith("http://")) return
        try {
            startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)))
        } catch (_: Exception) {
        }
    }

    // Мост для страницы: только то, чего WebView не умеет сам.
    inner class Bridge {
        @JavascriptInterface
        fun readClipboard(): String {
            val cm = getSystemService(Context.CLIPBOARD_SERVICE) as? ClipboardManager
            return cm?.primaryClip?.takeIf { it.itemCount > 0 }
                ?.getItemAt(0)?.coerceToText(this@MainActivity)?.toString().orEmpty()
        }

        @JavascriptInterface
        fun writeClipboard(text: String): Boolean = try {
            val cm = getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager
            cm.setPrimaryClip(ClipData.newPlainText("ssh_tunnel", text))
            true
        } catch (_: Exception) {
            false
        }

        @JavascriptInterface
        fun saveFile(name: String, text: String) = runOnUiThread {
            pendingSave = text
            saveFile.launch(name)
        }

        @JavascriptInterface
        fun scanQR() = runOnUiThread {
            val granted = ContextCompat.checkSelfPermission(this@MainActivity, Manifest.permission.CAMERA) ==
                PackageManager.PERMISSION_GRANTED
            if (granted) launchScanner() else cameraPermission.launch(Manifest.permission.CAMERA)
        }

        @JavascriptInterface
        fun openExternal(url: String) = runOnUiThread { this@MainActivity.openExternal(url) }

        /** Страница ядра своим экраном (Книга, приложения): путь с параметрами. */
        @JavascriptInterface
        fun openPage(url: String) = runOnUiThread {
            startActivity(Intent(this@MainActivity, WebPageActivity::class.java)
                .putExtra(WebPageActivity.EXTRA_URL, url))
        }

        @JavascriptInterface
        fun close() = runOnUiThread { moveTaskToBack(true) }
    }
}
