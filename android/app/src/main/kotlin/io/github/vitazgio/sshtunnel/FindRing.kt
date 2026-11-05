package io.github.vitazgio.sshtunnel

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.media.AudioAttributes
import android.media.AudioManager
import android.media.MediaPlayer
import android.media.RingtoneManager
import android.net.Uri
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.SystemClock

/**
 * «Найти телефон» с другого устройства. Свой трек (или будильник системы)
 * на громкости будильника, нарастает до максимума за 3 с, плюс вибрация.
 * Выключается: включил экран или разблокировал телефон, «Стоп» в
 * уведомлении, «Стоп» с другого устройства, сам через минуту.
 */
object FindRing {
    private const val ACTION_STOP = "io.github.vitazgio.sshtunnel.STOP_RING"
    private const val NOTIF_ID = 4016
    private const val PREFS = "phonelink"
    private val main = Handler(Looper.getMainLooper())

    private var player: MediaPlayer? = null
    private var vib: android.os.Vibrator? = null
    private var rx: BroadcastReceiver? = null
    private var startedAt = 0L
    private var oldAlarm = -1

    // ---- выбранный трек ----
    fun trackUri(c: Context): Uri? = c.getSharedPreferences(PREFS, 0).getString("ringUri", null)?.let { Uri.parse(it) }
    fun trackName(c: Context): String = c.getSharedPreferences(PREFS, 0).getString("ringName", "") ?: ""

    fun setTrack(c: Context, uri: Uri?) {
        val p = c.getSharedPreferences(PREFS, 0).edit()
        if (uri == null) {
            p.remove("ringUri").remove("ringName").apply()
            return
        }
        try {
            c.contentResolver.takePersistableUriPermission(uri, Intent.FLAG_GRANT_READ_URI_PERMISSION)
        } catch (_: Exception) {
        }
        var name = uri.lastPathSegment ?: "трек"
        try {
            c.contentResolver.query(uri, arrayOf(android.provider.OpenableColumns.DISPLAY_NAME), null, null, null)?.use {
                if (it.moveToFirst()) name = it.getString(0) ?: name
            }
        } catch (_: Exception) {
        }
        p.putString("ringUri", uri.toString()).putString("ringName", name).apply()
    }

    // ---- звонок ----
    fun start(ctx: Context) = main.post { startNow(ctx.applicationContext) }
    fun stop() = main.post { stopNow() }

    private fun prepare(c: Context, uri: Uri): MediaPlayer {
        val mp = MediaPlayer()
        mp.setAudioAttributes(
            AudioAttributes.Builder().setUsage(AudioAttributes.USAGE_ALARM)
                .setContentType(AudioAttributes.CONTENT_TYPE_MUSIC).build()
        )
        mp.setDataSource(c, uri)
        mp.isLooping = true
        mp.prepare()
        return mp
    }

    private fun startNow(c: Context) {
        ctx = c
        stopNow()
        val am = c.getSystemService(AudioManager::class.java)
        try {
            if (am != null) {
                oldAlarm = am.getStreamVolume(AudioManager.STREAM_ALARM)
                am.setStreamVolume(AudioManager.STREAM_ALARM, am.getStreamMaxVolume(AudioManager.STREAM_ALARM), 0)
            }
        } catch (_: Exception) {
        }
        val def = RingtoneManager.getDefaultUri(RingtoneManager.TYPE_ALARM)
            ?: RingtoneManager.getDefaultUri(RingtoneManager.TYPE_RINGTONE)
        val mp = try {
            prepare(c, trackUri(c) ?: def)
        } catch (_: Exception) {
            try { prepare(c, def) } catch (_: Exception) { null } // трек удалён — будильник
        }
        startedAt = SystemClock.uptimeMillis()
        if (mp != null) {
            mp.setVolume(0.05f, 0.05f)
            mp.start()
            player = mp
            // Нарастание: с тихого до максимума за 3 с.
            val ramp = object : Runnable {
                override fun run() {
                    if (player !== mp) return
                    val k = ((SystemClock.uptimeMillis() - startedAt) / 3000f).coerceIn(0.05f, 1f)
                    try { mp.setVolume(k, k) } catch (_: Exception) { return }
                    if (k < 1f) main.postDelayed(this, 100)
                }
            }
            main.post(ramp)
        }
        vib = if (Build.VERSION.SDK_INT >= 31)
            c.getSystemService(android.os.VibratorManager::class.java)?.defaultVibrator
        else @Suppress("DEPRECATION") c.getSystemService(android.os.Vibrator::class.java)
        try {
            vib?.vibrate(android.os.VibrationEffect.createWaveform(longArrayOf(0, 600, 400), 0))
        } catch (_: Exception) {
        }
        val r = object : BroadcastReceiver() {
            override fun onReceive(ctx: Context, i: Intent) {
                // Экран мог включиться от нашего же уведомления — первые 2 с
                // не считаем. Разблокировка и «Стоп» — всегда.
                if (i.action == Intent.ACTION_SCREEN_ON && SystemClock.uptimeMillis() - startedAt < 2000) return
                stopNow()
            }
        }
        val f = IntentFilter().apply {
            addAction(Intent.ACTION_SCREEN_ON)
            addAction(Intent.ACTION_USER_PRESENT)
            addAction(ACTION_STOP)
        }
        try {
            if (Build.VERSION.SDK_INT >= 33) c.registerReceiver(r, f, Context.RECEIVER_NOT_EXPORTED) else c.registerReceiver(r, f)
            rx = r
        } catch (_: Exception) {
        }
        notifyStop(c)
        main.postDelayed({ if (SystemClock.uptimeMillis() - startedAt >= 59_000) stopNow() }, 60_000)
    }

    private fun notifyStop(c: Context) {
        val nm = c.getSystemService(NotificationManager::class.java) ?: return
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            nm.createNotificationChannel(NotificationChannel("tools", c.getString(R.string.tools_channel), NotificationManager.IMPORTANCE_DEFAULT))
        }
        val stop = PendingIntent.getBroadcast(c, 0, Intent(ACTION_STOP).setPackage(c.packageName),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val n = Notification.Builder(c, "tools")
            .setSmallIcon(R.drawable.ic_notify)
            .setContentTitle(c.getString(R.string.phonelink_ring))
            .setContentText(c.getString(R.string.phonelink_ring_stop_hint))
            .setContentIntent(stop)
            .setDeleteIntent(stop)
            .addAction(Notification.Action.Builder(null, c.getString(R.string.phonelink_ring_stop), stop).build())
            .setOngoing(true)
            .build()
        try {
            nm.notify(NOTIF_ID, n)
        } catch (_: SecurityException) {
        }
    }

    private fun stopNow() {
        val c = rxCtx()
        player?.let { try { it.stop(); it.release() } catch (_: Exception) {} }
        player = null
        try { vib?.cancel() } catch (_: Exception) {}
        vib = null
        rx?.let { r -> try { c?.unregisterReceiver(r) } catch (_: Exception) {} }
        rx = null
        if (c != null) {
            c.getSystemService(NotificationManager::class.java)?.cancel(NOTIF_ID)
            if (oldAlarm >= 0) {
                try { c.getSystemService(AudioManager::class.java)?.setStreamVolume(AudioManager.STREAM_ALARM, oldAlarm, 0) } catch (_: Exception) {}
            }
        }
        oldAlarm = -1
    }

    @Volatile private var ctx: Context? = null
    fun init(c: Context) { ctx = c.applicationContext }
    private fun rxCtx(): Context? = ctx
}
