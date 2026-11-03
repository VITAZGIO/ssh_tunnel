package io.github.vitazgio.sshtunnel

import android.app.Activity

/**
 * Тема окна (оригинал, тёмная, светлая, консоль): цвет полос системы и фон
 * до загрузки страницы. Сам экран — общий HTML, оформление рисует он
 * (theme.css), так же, как в окне на компьютере.
 */
object Look {
    /** Ставит выбранную тему. Вызывать до setContentView. */
    fun apply(activity: Activity) {
        activity.setTheme(
            when (Settings(activity).theme) {
                "dark" -> R.style.AppTheme_Dark
                "light" -> R.style.AppTheme_Light
                "console" -> R.style.AppTheme_Console
                else -> R.style.AppTheme
            }
        )
    }
}
