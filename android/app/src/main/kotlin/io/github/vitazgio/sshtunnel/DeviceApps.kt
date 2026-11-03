package io.github.vitazgio.sshtunnel

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.provider.Settings as SysSettings
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/**
 * То, что дополнительным приложениям «устройств сети» (SSH, Файлы
 * устройства) нужно именно от телефона. Сами приложения — общие, в ядре Go.
 *
 * - Ключ SSH ядро шифрует ключом Android Keystore (KeystoreSealer): сам
 *   ключ Keystore из телефона не достать.
 * - Служба файлов работает, пока жив процесс (модули ядра запускает
 *   mobile.startUI) — поэтому её поднимает и служба VPN, а не только экран.
 * - Уведомление с прогрессом — только пока идёт передача, потом убирается.
 */
object DeviceApps {
    @Volatile private var ready = false

    fun init(ctx: Context) {
        if (ready) return
        synchronized(this) {
            if (ready) return
            val app = ctx.applicationContext
            mobile.Mobile.setKeySealer(KeystoreSealer)
            mobile.Mobile.setTransferListener(TransferNotifier(app))
            try {
                mobile.Mobile.startUI(app.filesDir.absolutePath)
            } catch (_: Exception) {
            }
            ready = true
        }
    }

    /** «Доступ ко всем файлам» — чтобы другие устройства видели Фото, Загрузки… */
    fun hasAllFiles(): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.R || Environment.isExternalStorageManager()

    fun requestAllFiles(ctx: Context) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) return
        val i = Intent(SysSettings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION)
            .setData(Uri.parse("package:" + ctx.packageName))
        try {
            ctx.startActivity(i)
        } catch (_: Exception) {
            ctx.startActivity(Intent(SysSettings.ACTION_MANAGE_ALL_FILES_ACCESS_PERMISSION))
        }
    }
}

/** AES-GCM ключом из Android Keystore: результат — 12 байт IV + шифр. */
object KeystoreSealer : mobile.KeySealer {
    private const val ALIAS = "ssh_tunnel_ssh_key"

    private fun key(): SecretKey {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        (ks.getKey(ALIAS, null) as? SecretKey)?.let { return it }
        val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore")
        gen.init(
            KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .build()
        )
        return gen.generateKey()
    }

    override fun seal(plain: ByteArray): ByteArray {
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.ENCRYPT_MODE, key())
        return c.iv + c.doFinal(plain)
    }

    override fun open(sealed: ByteArray): ByteArray {
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.DECRYPT_MODE, key(), GCMParameterSpec(128, sealed, 0, 12))
        return c.doFinal(sealed, 12, sealed.size - 12)
    }
}

/** Уведомление «Файлы устройства» с прогрессом; active == 0 — убрать. */
class TransferNotifier(private val ctx: Context) : mobile.TransferListener {
    private val nm = ctx.getSystemService(NotificationManager::class.java)

    override fun onTransfer(active: Long, name: String, done: Long, total: Long) {
        if (active <= 0L) {
            nm.cancel(ID)
            return
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            nm.createNotificationChannel(
                NotificationChannel(CHANNEL, ctx.getString(R.string.files_transfer_channel), NotificationManager.IMPORTANCE_LOW)
            )
        }
        val pct = if (total > 0) (done * 100 / total).toInt().coerceIn(0, 100) else 0
        val text = if (active > 1) ctx.getString(R.string.files_transfer_many, active.toInt()) else name
        val n = Notification.Builder(ctx, CHANNEL)
            .setSmallIcon(R.drawable.ic_notify)
            .setContentTitle(ctx.getString(R.string.files_transfer_title))
            .setContentText(text)
            .setProgress(100, pct, total <= 0)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .build()
        try {
            nm.notify(ID, n)
        } catch (_: SecurityException) {
            // Нет разрешения на уведомления — передача идёт и без него.
        }
    }

    companion object {
        private const val CHANNEL = "files"
        private const val ID = 4013
    }
}
