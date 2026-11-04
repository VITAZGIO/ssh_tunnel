package io.github.vitazgio.sshtunnel

import android.app.Activity
import android.app.AlertDialog
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.widget.Toast
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * Записка о падении. Без неё «зависает и вылетает» не разобрать: logcat с
 * телефона владельца не достать. Падение Kotlin пишет обработчик ниже
 * (crash.txt), падение ядра Go — само ядро (crash-go.txt, см.
 * mobile.StartUI). При следующем запуске главный экран показывает записку
 * с кнопкой «Скопировать» — её можно переслать разработчику.
 */
object CrashLog {
    private const val KOTLIN = "crash.txt"
    private const val GO = "crash-go-prev.txt" // ядро переносит сюда прошлую записку

    fun install(ctx: Context) {
        val dir = ctx.applicationContext.filesDir
        val prev = Thread.getDefaultUncaughtExceptionHandler()
        if (prev is Handler) return
        Thread.setDefaultUncaughtExceptionHandler(Handler(dir, prev))
    }

    private class Handler(val dir: File, val prev: Thread.UncaughtExceptionHandler?) :
        Thread.UncaughtExceptionHandler {
        override fun uncaughtException(t: Thread, e: Throwable) {
            try {
                val time = SimpleDateFormat("yyyy-MM-dd HH:mm:ss", Locale.US).format(Date())
                File(dir, KOTLIN).writeText(
                    "$time ${BuildConfig.VERSION_NAME}, поток ${t.name}\n${e.stackTraceToString()}".take(20000)
                )
            } catch (_: Exception) {
            }
            prev?.uncaughtException(t, e)
        }
    }

    /** Прошлый запуск упал — показать записку (один раз) и удалить её. */
    fun showIfAny(a: Activity) {
        val text = listOf(KOTLIN, GO).mapNotNull { name ->
            val f = File(a.filesDir, name)
            val s = try { if (f.length() > 0) f.readText().trim() else "" } catch (_: Exception) { "" }
            f.delete()
            s.ifEmpty { null }
        }.joinToString("\n\n").take(20000)
        if (text.isEmpty()) return
        AlertDialog.Builder(a)
            .setTitle(R.string.crash_title)
            .setMessage(text.take(3000))
            .setPositiveButton(R.string.crash_copy) { _, _ ->
                a.getSystemService(ClipboardManager::class.java)
                    ?.setPrimaryClip(ClipData.newPlainText("ssh_tunnel crash", text))
                Toast.makeText(a, R.string.crash_copied, Toast.LENGTH_SHORT).show()
            }
            .setNegativeButton(android.R.string.cancel, null)
            .show()
    }
}
