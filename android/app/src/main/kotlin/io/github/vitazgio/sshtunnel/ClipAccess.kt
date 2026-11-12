package io.github.vitazgio.sshtunnel

import android.accessibilityservice.AccessibilityService
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.view.accessibility.AccessibilityEvent

/**
 * ТЗ 4: «Общий буфер» без кнопок. Android (10+) отдаёт буфер только
 * программе с фокусом. Служба спецвозможностей замечает нажатие «Копировать»
 * в любом приложении и на мгновение открывает прозрачное ClipReadActivity —
 * оно получает фокус, читает буфер и отправляет его (тем же путём, что плитка).
 *
 * Выключена, пока человек сам не включит (Настройки → Спецвозможности).
 * Берёт только события нажатия и из них — подпись нажатой кнопки; дерево
 * экрана, тексты полей и пароли недоступны (canRetrieveWindowContent=false).
 * Плагин «Общий буфер» не добавлен/выключен — ничего не делает.
 */
class ClipAccess : AccessibilityService() {
    private val main = Handler(Looper.getMainLooper())
    private var last = 0L

    override fun onAccessibilityEvent(e: AccessibilityEvent?) {
        if (e == null || e.eventType != AccessibilityEvent.TYPE_VIEW_CLICKED) return
        if (e.packageName == packageName) return
        val label = StringBuilder()
        e.text?.forEach { label.append(it).append(' ') }
        e.contentDescription?.let { label.append(it) }
        val l = label.toString().lowercase()
        if (WORDS.none { l.contains(it) }) return
        val now = System.currentTimeMillis()
        if (now - last < 1500) return
        last = now
        if (!(try { mobile.Mobile.clipActive() } catch (_: Throwable) { false })) return
        // Дать приложению положить данные в буфер, и только потом читать.
        main.postDelayed({
            try {
                startActivity(Intent(this, ClipReadActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            } catch (_: Exception) {
            }
        }, 300)
    }

    override fun onInterrupt() {}

    companion object {
        /** Подписи кнопок «Копировать» (en, ru, uk): достаточно начала слова. */
        private val WORDS = listOf("copy", "копир", "скопир", "копіюв")

        fun enabled(c: Context): Boolean {
            val s = Settings.Secure.getString(c.contentResolver, Settings.Secure.ENABLED_ACCESSIBILITY_SERVICES) ?: return false
            val me = ComponentName(c, ClipAccess::class.java)
            return s.split(':').any { ComponentName.unflattenFromString(it) == me }
        }

        fun openSettings(c: Context) {
            try {
                c.startActivity(Intent(Settings.ACTION_ACCESSIBILITY_SETTINGS).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            } catch (_: Exception) {
            }
        }

        /** Страница приложения: там, на Android 13+, в меню ⋮ есть «Разрешить
         * ограниченные настройки» — без этого установленное вручную приложение
         * не даст включить спецвозможность. */
        fun openAppInfo(c: Context) {
            try {
                c.startActivity(
                    Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:" + c.packageName))
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                )
            } catch (_: Exception) {
            }
        }
    }
}
