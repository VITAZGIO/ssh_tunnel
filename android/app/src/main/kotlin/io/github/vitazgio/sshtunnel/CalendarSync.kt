package io.github.vitazgio.sshtunnel

import android.accounts.AbstractAccountAuthenticator
import android.accounts.Account
import android.accounts.AccountAuthenticatorResponse
import android.accounts.AccountManager
import android.app.Service
import android.content.AbstractThreadedSyncAdapter
import android.content.ContentProviderClient
import android.content.ContentProviderOperation
import android.content.ContentResolver
import android.content.Context
import android.content.Intent
import android.content.SyncResult
import android.graphics.Color
import android.net.Uri
import android.os.Bundle
import android.os.IBinder
import android.provider.CalendarContract
import android.provider.CalendarContract.Calendars
import android.provider.CalendarContract.Events
import android.provider.ContactsContract
import android.provider.ContactsContract.CommonDataKinds
import android.provider.ContactsContract.RawContacts
import org.json.JSONArray
import org.json.JSONObject
import java.text.SimpleDateFormat
import java.util.Locale
import java.util.TimeZone
import java.util.concurrent.Executors

// Календарь на телефоне (ТЗ 7). Сверку с Nextcloud делает ядро на Go
// (mobile.CalendarSyncNow), здесь — только перенос готового в систему:
// свой аккаунт, SyncAdapter (его система будит раз в 15 минут и после
// перезагрузки), события — в CalendarContract (видно в любом календаре),
// контакты — в ContactsContract (если включена галочка). Системный
// календарь только для чтения: события правятся в самом приложении — так
// правка сразу идёт на сервер и конфликты решает он.
object CalendarSync {
    const val ACCOUNT_TYPE = "io.github.vitazgio.sshtunnel.calendar"
    private const val PERIOD_SEC = 15 * 60L
    private const val PREFS = "calendar_sync"

    private val bg = Executors.newSingleThreadExecutor()
    @Volatile private var inited = false

    fun account(ctx: Context) = Account(ctx.getString(R.string.app_name), ACCOUNT_TYPE)

    // init — ядро страниц (там календарь), часовой пояс и подписка на
    // изменения: правка в приложении сразу переписывает системный календарь
    // и виджет, без похода в сеть.
    @Synchronized
    fun init(ctx: Context) {
        val app = ctx.applicationContext
        mobile.Mobile.setTimeZone(TimeZone.getDefault().id)
        try {
            mobile.Mobile.startUI(app.filesDir.absolutePath)
        } catch (_: Exception) {
            return
        }
        if (inited) return
        inited = true
        mobile.Mobile.setCalendarListener(object : mobile.CalendarListener {
            override fun calendarChanged() {
                bg.execute { writeIfChanged(app) }
            }
        })
    }

    // setup — после подключения на странице: аккаунт в системе, периодическая
    // синхронизация календаря и (по галочке) контактов.
    fun setup(ctx: Context, contacts: Boolean) {
        init(ctx)
        val acc = account(ctx)
        val am = AccountManager.get(ctx)
        if (am.getAccountsByType(ACCOUNT_TYPE).none { it == acc }) {
            am.addAccountExplicitly(acc, null, null)
        }
        for ((auth, on) in listOf(CalendarContract.AUTHORITY to true, ContactsContract.AUTHORITY to contacts)) {
            ContentResolver.setIsSyncable(acc, auth, if (on) 1 else 0)
            ContentResolver.setSyncAutomatically(acc, auth, on)
            if (on) ContentResolver.addPeriodicSync(acc, auth, Bundle.EMPTY, PERIOD_SEC)
            else ContentResolver.removePeriodicSync(acc, auth, Bundle.EMPTY)
        }
        if (!contacts) bg.execute { clearContacts(ctx.applicationContext) }
        requestSync(ctx)
    }

    // forget — календарь отключён: аккаунт убирается, система сама удаляет
    // его календари и контакты.
    fun forget(ctx: Context) {
        val am = AccountManager.get(ctx)
        am.getAccountsByType(ACCOUNT_TYPE).forEach { am.removeAccountExplicitly(it) }
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().clear().apply()
        CalendarWidget.updateAll(ctx)
    }

    fun requestSync(ctx: Context) {
        val b = Bundle().apply {
            putBoolean(ContentResolver.SYNC_EXTRAS_MANUAL, true)
            putBoolean(ContentResolver.SYNC_EXTRAS_EXPEDITED, true)
        }
        ContentResolver.requestSync(account(ctx), CalendarContract.AUTHORITY, b)
    }

    // sync — из SyncAdapter: сверка в ядре, потом перенос в систему.
    fun sync(ctx: Context, result: SyncResult?) {
        init(ctx)
        val r = JSONObject(mobile.Mobile.calendarSyncNow(ctx.filesDir.absolutePath))
        if (!r.optBoolean("active")) return
        if (r.has("error")) result?.let { it.stats.numIoExceptions++ }
        writeIfChanged(ctx, force = true)
    }

    @Synchronized
    private fun writeIfChanged(ctx: Context, force: Boolean = false) {
        val prefs = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val rev = mobile.Mobile.calendarRev()
        if (!force && prefs.getLong("rev", -1) == rev) return
        if (AccountManager.get(ctx).getAccountsByType(ACCOUNT_TYPE).isEmpty()) {
            CalendarWidget.updateAll(ctx)
            return
        }
        try {
            writeCalendars(ctx)
            if (ContentResolver.getIsSyncable(account(ctx), ContactsContract.AUTHORITY) > 0) writeContacts(ctx)
            prefs.edit().putLong("rev", rev).apply()
        } catch (_: SecurityException) {
            // Разрешение на календарь не дали — виджет всё равно работает.
        }
        CalendarWidget.updateAll(ctx)
    }

    private fun asSync(uri: Uri, acc: Account): Uri = uri.buildUpon()
        .appendQueryParameter(CalendarContract.CALLER_IS_SYNCADAPTER, "true")
        .appendQueryParameter(Calendars.ACCOUNT_NAME, acc.name)
        .appendQueryParameter(Calendars.ACCOUNT_TYPE, acc.type).build()

    private fun color(s: String): Int = try {
        Color.parseColor(s)
    } catch (_: Exception) {
        0xFF4C8DFF.toInt()
    }

    private val utcFmt = SimpleDateFormat("yyyyMMdd'T'HHmmss'Z'", Locale.US).apply { timeZone = TimeZone.getTimeZone("UTC") }

    // writeCalendars — календари и события ядра → CalendarContract. Ключ
    // строки — href события (+ время изменённого повтора), ETag — в
    // SYNC_DATA1: переписываются только изменившиеся.
    private fun writeCalendars(ctx: Context) {
        val acc = account(ctx)
        val cr = ctx.contentResolver
        val cals = JSONArray(mobile.Mobile.calendarExportJSON())
        val calUri = asSync(Calendars.CONTENT_URI, acc)

        // Календари: href → _ID.
        val have = HashMap<String, Long>()
        cr.query(calUri, arrayOf(Calendars._ID, Calendars._SYNC_ID),
            "${Calendars.ACCOUNT_NAME}=? AND ${Calendars.ACCOUNT_TYPE}=?", arrayOf(acc.name, acc.type), null)?.use {
            while (it.moveToNext()) have[it.getString(1) ?: ""] = it.getLong(0)
        }
        val seen = HashSet<String>()
        for (i in 0 until cals.length()) {
            val c = cals.getJSONObject(i)
            val href = c.getString("Href")
            seen += href
            val v = android.content.ContentValues().apply {
                put(Calendars.NAME, c.getString("Name"))
                put(Calendars.CALENDAR_DISPLAY_NAME, c.getString("Name"))
                put(Calendars.CALENDAR_COLOR, color(c.optString("Color")))
                put(Calendars.CALENDAR_ACCESS_LEVEL, Calendars.CAL_ACCESS_READ)
                put(Calendars.VISIBLE, 1)
                put(Calendars.SYNC_EVENTS, 1)
            }
            var id = have[href]
            if (id != null) {
                cr.update(asSync(Calendars.CONTENT_URI.buildUpon().appendPath(id.toString()).build(), acc), v, null, null)
            } else {
                v.put(Calendars.ACCOUNT_NAME, acc.name)
                v.put(Calendars.ACCOUNT_TYPE, acc.type)
                v.put(Calendars._SYNC_ID, href)
                v.put(Calendars.OWNER_ACCOUNT, acc.name)
                v.put(Calendars.CALENDAR_TIME_ZONE, TimeZone.getDefault().id)
                id = cr.insert(calUri, v)?.lastPathSegment?.toLongOrNull() ?: continue
            }
            writeEvents(ctx, acc, id, c.getJSONArray("Events"))
        }
        for ((href, id) in have) if (href !in seen) {
            cr.delete(asSync(Calendars.CONTENT_URI.buildUpon().appendPath(id.toString()).build(), acc), null, null)
        }
    }

    private fun evKey(e: JSONObject) = e.getString("Href") + "#" + e.optLong("RecurrenceID")

    private fun writeEvents(ctx: Context, acc: Account, calId: Long, evs: JSONArray) {
        val cr = ctx.contentResolver
        val evUri = asSync(Events.CONTENT_URI, acc)
        val have = HashMap<String, Pair<Long, String>>() // ключ → (_ID, etag)
        cr.query(evUri, arrayOf(Events._ID, Events._SYNC_ID, Events.SYNC_DATA1),
            "${Events.CALENDAR_ID}=?", arrayOf(calId.toString()), null)?.use {
            while (it.moveToNext()) have[it.getString(1) ?: ""] = it.getLong(0) to (it.getString(2) ?: "")
        }
        // Главные события раньше изменённых повторов: повтор ссылается на них.
        val list = (0 until evs.length()).map { evs.getJSONObject(it) }.sortedBy { it.optLong("RecurrenceID") != 0L }
        val ops = ArrayList<ContentProviderOperation>()
        val seen = HashSet<String>()
        for (e in list) {
            val k = evKey(e)
            seen += k
            val old = have[k]
            if (old != null && old.second == e.optString("ETag") && e.optString("ETag").isNotEmpty()) continue
            if (old != null) ops += ContentProviderOperation.newDelete(asSync(Events.CONTENT_URI.buildUpon().appendPath(old.first.toString()).build(), acc)).build()
            ops += ContentProviderOperation.newInsert(evUri).withValues(eventValues(calId, e)).build()
        }
        for ((k, v) in have) if (k !in seen) {
            ops += ContentProviderOperation.newDelete(asSync(Events.CONTENT_URI.buildUpon().appendPath(v.first.toString()).build(), acc)).build()
        }
        ops.chunked(200).forEach { cr.applyBatch(CalendarContract.AUTHORITY, ArrayList(it)) }
    }

    private fun eventValues(calId: Long, e: JSONObject) = android.content.ContentValues().apply {
        val allDay = e.optBoolean("AllDay")
        val start = e.getLong("Start")
        val end = maxOf(e.getLong("End"), start)
        val rrule = e.optString("RRule")
        put(Events.CALENDAR_ID, calId)
        put(Events._SYNC_ID, evKey(e))
        put(Events.SYNC_DATA1, e.optString("ETag"))
        put(Events.TITLE, e.optString("Title"))
        put(Events.EVENT_LOCATION, e.optString("Location"))
        put(Events.DESCRIPTION, e.optString("Description"))
        put(Events.DTSTART, start)
        put(Events.ALL_DAY, if (allDay) 1 else 0)
        // У события на весь день пояс обязан быть UTC.
        put(Events.EVENT_TIMEZONE, if (allDay) "UTC" else e.optString("TZ").ifEmpty { "UTC" })
        if (rrule.isNotEmpty() && e.optLong("RecurrenceID") == 0L) {
            // Повторяющемуся событию система требует DURATION, а не DTEND.
            put(Events.RRULE, rrule)
            put(Events.DURATION, if (allDay) "P${maxOf(1, (end - start) / 86_400_000)}D" else "P${(end - start) / 1000}S")
            val ex = e.optJSONArray("ExDates")
            if (ex != null && ex.length() > 0) {
                put(Events.EXDATE, (0 until ex.length()).joinToString(",") { utcFmt.format(java.util.Date(ex.getLong(it))) })
            }
        } else {
            put(Events.DTEND, end)
        }
        val rid = e.optLong("RecurrenceID")
        if (rid != 0L) {
            put(Events.ORIGINAL_SYNC_ID, e.getString("Href") + "#0")
            put(Events.ORIGINAL_INSTANCE_TIME, rid)
            put(Events.ORIGINAL_ALL_DAY, if (allDay) 1 else 0)
        }
        if (e.optBoolean("Cancelled")) put(Events.STATUS, Events.STATUS_CANCELED)
    }

    // ── контакты ─────────────────────────────────────────────────────────
    private fun contactsUri(uri: Uri, acc: Account): Uri = uri.buildUpon()
        .appendQueryParameter(ContactsContract.CALLER_IS_SYNCADAPTER, "true")
        .appendQueryParameter(RawContacts.ACCOUNT_NAME, acc.name)
        .appendQueryParameter(RawContacts.ACCOUNT_TYPE, acc.type).build()

    private fun clearContacts(ctx: Context) {
        val acc = account(ctx)
        try {
            ctx.contentResolver.delete(contactsUri(RawContacts.CONTENT_URI, acc),
                "${RawContacts.ACCOUNT_NAME}=? AND ${RawContacts.ACCOUNT_TYPE}=?", arrayOf(acc.name, acc.type))
        } catch (_: SecurityException) {
        }
    }

    // writeContacts — контакты пишутся заново целиком: их мало, а сверка
    // по полям ради этого не нужна.
    private fun writeContacts(ctx: Context) {
        val acc = account(ctx)
        clearContacts(ctx)
        val list = JSONArray(mobile.Mobile.contactsExportJSON())
        val ops = ArrayList<ContentProviderOperation>()
        val dataUri = contactsUri(ContactsContract.Data.CONTENT_URI, acc)
        for (i in 0 until list.length()) {
            val c = list.getJSONObject(i)
            val back = ops.size
            ops += ContentProviderOperation.newInsert(contactsUri(RawContacts.CONTENT_URI, acc))
                .withValue(RawContacts.ACCOUNT_NAME, acc.name)
                .withValue(RawContacts.ACCOUNT_TYPE, acc.type)
                .withValue(RawContacts.SOURCE_ID, c.optString("uid")).build()
            fun data(mime: String, vararg kv: Pair<String, String>) {
                val b = ContentProviderOperation.newInsert(dataUri)
                    .withValueBackReference(ContactsContract.Data.RAW_CONTACT_ID, back)
                    .withValue(ContactsContract.Data.MIMETYPE, mime)
                kv.forEach { b.withValue(it.first, it.second) }
                ops += b.build()
            }
            data(CommonDataKinds.StructuredName.CONTENT_ITEM_TYPE, CommonDataKinds.StructuredName.DISPLAY_NAME to c.optString("name"))
            c.optJSONArray("phones")?.let { a -> for (j in 0 until a.length()) data(CommonDataKinds.Phone.CONTENT_ITEM_TYPE, CommonDataKinds.Phone.NUMBER to a.getString(j)) }
            c.optJSONArray("emails")?.let { a -> for (j in 0 until a.length()) data(CommonDataKinds.Email.CONTENT_ITEM_TYPE, CommonDataKinds.Email.ADDRESS to a.getString(j)) }
            if (c.optString("org").isNotEmpty()) data(CommonDataKinds.Organization.CONTENT_ITEM_TYPE, CommonDataKinds.Organization.COMPANY to c.optString("org"))
            // Одна пачка — до ~400 операций, и на границе контакта.
            if (ops.size > 400) {
                ctx.contentResolver.applyBatch(ContactsContract.AUTHORITY, ArrayList(ops))
                ops.clear()
            }
        }
        if (ops.isNotEmpty()) ctx.contentResolver.applyBatch(ContactsContract.AUTHORITY, ops)
    }
}

// Аккаунт календаря в системе: без «аутентификатора» Android не даёт завести
// свой аккаунт. Пароль здесь не хранится — он у ядра, зашифрованным.
class CalendarAuthService : Service() {
    private val auth by lazy {
        object : AbstractAccountAuthenticator(this) {
            override fun editProperties(r: AccountAuthenticatorResponse?, t: String?) = null
            override fun addAccount(r: AccountAuthenticatorResponse?, t: String?, a: String?, f: Array<out String>?, o: Bundle?) =
                Bundle().apply {
                    // «Добавить аккаунт» из настроек системы — открыть страницу календаря.
                    putParcelable(AccountManager.KEY_INTENT, Intent(this@CalendarAuthService, WebPageActivity::class.java)
                        .putExtra(WebPageActivity.EXTRA_PAGE, WebPageActivity.PAGE_APPS)
                        .putExtra(WebPageActivity.EXTRA_OPEN, "calendar"))
                }
            override fun confirmCredentials(r: AccountAuthenticatorResponse?, a: Account?, o: Bundle?) = null
            override fun getAuthToken(r: AccountAuthenticatorResponse?, a: Account?, t: String?, o: Bundle?) = null
            override fun getAuthTokenLabel(t: String?) = null
            override fun updateCredentials(r: AccountAuthenticatorResponse?, a: Account?, t: String?, o: Bundle?) = null
            override fun hasFeatures(r: AccountAuthenticatorResponse?, a: Account?, f: Array<out String>?) =
                Bundle().apply { putBoolean(AccountManager.KEY_BOOLEAN_RESULT, false) }
        }
    }

    override fun onBind(intent: Intent?): IBinder? = auth.iBinder
}

// SyncAdapter календаря и контактов: система будит его по расписанию
// (раз в 15 минут), после перезагрузки и по requestSync.
open class CalendarSyncService : Service() {
    private val adapter by lazy {
        object : AbstractThreadedSyncAdapter(applicationContext, true) {
            override fun onPerformSync(account: Account, extras: Bundle, authority: String,
                                       provider: ContentProviderClient, syncResult: SyncResult) {
                CalendarSync.sync(context, syncResult)
            }
        }
    }

    override fun onBind(intent: Intent?): IBinder? = adapter.syncAdapterBinder
}

// Тот же SyncAdapter для контактов (у каждого authority своё описание в
// манифесте).
class ContactsSyncService : CalendarSyncService()
