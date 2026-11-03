package io.github.vitazgio.sshtunnel

import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.widget.RemoteViews
import org.json.JSONArray
import java.text.DateFormat
import java.util.Calendar
import java.util.Date
import java.util.TimeZone

// Виджет календаря на главном экране (ТЗ 7): сегодняшняя дата и ближайшие
// события из ядра (mobile.CalendarUpcomingJSON). Обновляется после каждой
// сверки и правки (CalendarSync) и раз в полчаса сам; нажатие открывает
// календарь в приложении.
class CalendarWidget : AppWidgetProvider() {

    override fun onUpdate(ctx: Context, mgr: AppWidgetManager, ids: IntArray) {
        CalendarSync.init(ctx)
        val views = build(ctx)
        ids.forEach { mgr.updateAppWidget(it, views) }
    }

    companion object {
        private const val MAX_ROWS = 8

        fun updateAll(ctx: Context) {
            val mgr = AppWidgetManager.getInstance(ctx)
            val ids = mgr.getAppWidgetIds(ComponentName(ctx, CalendarWidget::class.java))
            if (ids.isEmpty()) return
            val views = build(ctx)
            ids.forEach { mgr.updateAppWidget(it, views) }
        }

        private fun build(ctx: Context): RemoteViews {
            val rv = RemoteViews(ctx.packageName, R.layout.widget_calendar)
            rv.setTextViewText(R.id.wDate, DateFormat.getDateInstance(DateFormat.FULL).format(Date()))
            rv.removeAllViews(R.id.wList)
            val items = try {
                JSONArray(mobile.Mobile.calendarUpcomingJSON(7))
            } catch (_: Exception) {
                JSONArray()
            }
            val time = DateFormat.getTimeInstance(DateFormat.SHORT)
            val day = java.text.SimpleDateFormat("EE d MMM", java.util.Locale.getDefault())
            val today = Calendar.getInstance()
            for (i in 0 until minOf(items.length(), MAX_ROWS)) {
                val e = items.getJSONObject(i)
                val row = RemoteViews(ctx.packageName, R.layout.widget_calendar_row)
                val start = Date(e.getLong("start"))
                val allDay = e.optBoolean("allDay")
                // Событие на весь день — полночь UTC: дату показываем по UTC.
                val dayFmt = if (allDay) (day.clone() as java.text.SimpleDateFormat).apply { timeZone = TimeZone.getTimeZone("UTC") } else day
                val c = Calendar.getInstance().apply { this.time = start }
                val sameDay = !allDay && c.get(Calendar.YEAR) == today.get(Calendar.YEAR) &&
                    c.get(Calendar.DAY_OF_YEAR) == today.get(Calendar.DAY_OF_YEAR)
                val whenText = when {
                    allDay -> dayFmt.format(start)
                    sameDay -> time.format(start)
                    else -> dayFmt.format(start) + " " + time.format(start)
                }
                row.setTextViewText(R.id.wWhen, whenText)
                row.setTextViewText(R.id.wTitle, e.optString("title"))
                val col = try { Color.parseColor(e.optString("color")) } catch (_: Exception) { 0xFF4C8DFF.toInt() }
                row.setInt(R.id.wBar, "setBackgroundColor", col)
                rv.addView(R.id.wList, row)
            }
            rv.setViewVisibility(R.id.wEmpty, if (items.length() == 0) android.view.View.VISIBLE else android.view.View.GONE)
            val open = Intent(ctx, WebPageActivity::class.java)
                .putExtra(WebPageActivity.EXTRA_PAGE, WebPageActivity.PAGE_APPS)
                .putExtra(WebPageActivity.EXTRA_OPEN, "calendar")
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            rv.setOnClickPendingIntent(R.id.wRoot, PendingIntent.getActivity(ctx, 0, open,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE))
            return rv
        }
    }
}
