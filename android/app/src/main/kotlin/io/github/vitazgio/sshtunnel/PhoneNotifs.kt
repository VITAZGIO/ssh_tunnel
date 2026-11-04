package io.github.vitazgio.sshtunnel

import android.app.Notification
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.provider.Settings
import android.service.notification.NotificationListenerService
import android.service.notification.StatusBarNotification

/**
 * «Связь с телефоном»: уведомления телефона — на свои устройства (ПК
 * покажет их у часов). Android даёт их только службе, которой человек сам
 * разрешил «Доступ к уведомлениям» в настройках системы. Пересылает ядро
 * (mobile.PhoneLinkNotify) — только если плагин добавлен и пересылка включена.
 */
class PhoneNotifs : NotificationListenerService() {
    override fun onNotificationPosted(sbn: StatusBarNotification) {
        val n = sbn.notification ?: return
        // Свои, постоянные («идёт загрузка», музыка) и сводки групп — не шлём.
        if (sbn.packageName == packageName || sbn.isOngoing) return
        if (n.flags and Notification.FLAG_GROUP_SUMMARY != 0) return
        val ex = n.extras ?: return
        val title = ex.getCharSequence(Notification.EXTRA_TITLE)?.toString() ?: ""
        val text = (ex.getCharSequence(Notification.EXTRA_BIG_TEXT) ?: ex.getCharSequence(Notification.EXTRA_TEXT))?.toString() ?: ""
        if (title.isEmpty() && text.isEmpty()) return
        val app = try {
            packageManager.getApplicationLabel(packageManager.getApplicationInfo(sbn.packageName, 0)).toString()
        } catch (_: Exception) {
            sbn.packageName
        }
        Thread { mobile.Mobile.phoneLinkNotify(app, title, text.take(1000)) }.start()
    }

    companion object {
        fun allowed(c: Context): Boolean {
            val s = Settings.Secure.getString(c.contentResolver, "enabled_notification_listeners") ?: return false
            return s.contains(ComponentName(c, PhoneNotifs::class.java).flattenToString())
        }

        fun openSettings(c: Context) {
            try {
                c.startActivity(Intent(Settings.ACTION_NOTIFICATION_LISTENER_SETTINGS).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            } catch (_: Exception) {
            }
        }
    }
}
