package io.github.vitazgio.sshtunnel

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.BroadcastReceiver
import android.content.IntentFilter
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.VpnService
import android.net.wifi.WifiManager
import android.os.Build
import android.os.ParcelFileDescriptor
import android.util.Log
import mobile.Callbacks
import mobile.Mobile
import org.json.JSONObject
import java.net.InetAddress

/**
 * Служба туннеля.
 *
 * Здесь происходит то, чего нет и не может быть на компьютере: система отдаёт
 * виртуальный сетевой интерфейс, в который сыплются сырые IP-пакеты, а разбирать
 * их и превращать обратно в соединения — забота ядра на Go.
 */
class TunnelService : VpnService(), Callbacks {

    companion object {
        const val ACTION_START = "io.github.vitazgio.sshtunnel.START"
        const val ACTION_STOP = "io.github.vitazgio.sshtunnel.STOP"
        /**
         * Две кнопки главного экрана, как на компьютере: EXTRA_BYPASS — обход
         * блокировок (сайты через сервер), EXTRA_MESH — сеть устройств.
         * Туннель держится, пока нужна хотя бы одна; обе выключены — остановка.
         */
        const val ACTION_MODES = "io.github.vitazgio.sshtunnel.MODES"
        const val EXTRA_BYPASS = "bypass"
        const val EXTRA_MESH = "mesh"

        /** Что включено сейчас (или с чем поднимается туннель). */
        @Volatile var bypassOn: Boolean = true
        @Volatile var meshOn: Boolean = true

        /** Состояние и журнал для экрана: служба живёт отдельно от него. */
        @Volatile var state: String = "stopped"
        @Volatile var detail: String = ""

        /**
         * Туннель поднят без сервера ради сети устройств (ТЗ 11): почему
         * сервер не ответил (текст как есть и код причины, см.
         * mobile.ServerDownKind). Большая кнопка показывает это ошибкой, как
         * на ПК; сеть устройств при этом работает. Пусто — сервер был на связи.
         */
        @Volatile var serverDown: String = ""
        @Volatile var serverDownKind: String = ""

        /**
         * Обход погас сам, потому что сервер не ответил (а человек его с тех
         * пор не нажимал): когда сервер вернётся, обход включится обратно, как
         * на ПК (core.Machine, ТЗ 20). Любое нажатие человека это снимает.
         */
        @Volatile var bypassAutoOff: Boolean = false

        // Нажатия человека по счёту и замок вокруг tunnel.setModes: возврат
        // обхода сам по себе не должен лечь поверх нажатия, сделанного в тот
        // же миг (см. serverBack).
        @Volatile private var humanSeq = 0L
        private val modesLock = Any()

        /** Цепочка выхода для экрана (mobile.ExitJSON); пусто — её нет. */
        @Volatile var exitJson: String = ""

        /**
         * Список серверов поменялся (экран сохранил настройки): дополнительные
         * серверы сети устройств — сразу, цепочка — со следующим подключением.
         */
        fun profilesChanged(s: Settings) {
            val svc = current ?: return
            val json = s.coreProfilesJson()
            val active = s.active().id
            val udp = s.udpRelayEnabled
            Thread {
                try {
                    svc.tunnel.setProfiles(json, active, udp)
                } catch (e: Exception) {
                    svc.onLog("серверы: ${e.message}")
                }
            }.start()
        }

        /** Сеть устройств одной строкой (mobile.MeshSummary). */
        @Volatile var meshSummary: String = """{"state":"off"}"""
        @Volatile var statsJson: String = "{}"
        val log = ArrayDeque<String>()

        /** Плитка в шторке, пока шторка открыта. */
        @Volatile var onUpdate: (() -> Unit)? = null

        /** Общий экран (AndroidApi): состояние и строки журнала. */
        @Volatile var onApiUpdate: (() -> Unit)? = null
        @Volatile var onApiLog: ((String) -> Unit)? = null

        /** Внешний адрес через туннель (JSON, см. mobile.Tunnel.CheckIP). */
        fun checkIP(): String =
            current?.tunnel?.checkIP() ?: """{"error":"туннель не запущен"}"""

        /** Работающая служба — через неё экран запускает тест скорости. */
        @Volatile private var current: TunnelService? = null

        fun speedTest(): String =
            current?.tunnel?.speedTest() ?: """{"error":"туннель выключен"}"""

        /** Создание пользователя на сервере для экрана (/api/serverusers, JSON, см. mobile.ServerUsers). */
        fun serverUsers(op: String, name: String, type: String, pass: String): String =
            current?.tunnel?.serverUsers(op, name, type, pass)
                ?: """{"error":"туннель выключен"}"""

        /** Состояние сети устройств для экрана (/api/mesh, JSON, см. mobile.MeshStatusJSON). */
        fun meshStatus(): String =
            current?.tunnel?.meshStatusJSON() ?: """{"state":"stopped"}"""

        /**
         * Самопроверка не требует поднятого туннеля — соединение для неё
         * отдельное — но всё равно идёт через уже настроенный tunnel, чтобы
         * не заводить второй экземпляр ядра и второй набор настроек.
         * Настройки, если служба не запущена, применяются здесь же, прямо
         * перед вызовом: Configure сам по себе ничего не поднимает.
         */
        fun selfCheck(context: Context): String {
            val svc = current
            if (svc != null) return svc.tunnel.selfCheck()

            val settings = Settings(context)
            if (!settings.ready()) return """{"error":"не заполнены настройки"}"""
            val tmp = Mobile.newTunnel()
            return try {
                tmp.configure(
                    settings.host, settings.sshPort.toLong(), settings.user,
                    settings.keyFile.absolutePath, settings.knownHostsFile.absolutePath,
                    settings.poolSize.toLong(), settings.directHosts, settings.localViaTunnel,
                    settings.adBlockEnabled, settings.adBlockListFile.absolutePath, settings.adBlockAllowlist,
                    settings.udpRelayEnabled,
                )
                tmp.selfCheck()
            } catch (e: Exception) {
                """{"error":"${e.message}"}"""
            }
        }

        private const val TAG = "ssh_tunnel"
        private const val CHANNEL = "tunnel"
        private const val NOTIFICATION_ID = 1
        private const val MTU = 1500

        /**
         * Сколько секунд после «Отключить» интерфейс VPN ещё живёт, ведя
         * трафик напрямую (см. Mobile.Tunnel.Drain и docs/DRAIN_SPEC.md).
         *
         * Меньше, чем сорок пять секунд на компьютере, и намеренно: пока
         * интерфейс поднят, система показывает в статусной строке свой значок
         * VPN — даже когда наше приложение уже написало «выключено». Двадцати
         * секунд хватает, чтобы браузер и мессенджер пережили выключение и
         * чтобы человек успел передумать, а вводить в заблуждение значком
         * надолго не хочется.
         */
        private const val DRAIN_SECONDS = 20

        /** Сколько ждать остановки ядра, прежде чем закрыться без неё. */
        private const val STOP_WAIT_MS = 4000L
    }

    private var fd: ParcelFileDescriptor? = null
    private val tunnel = Mobile.newTunnel()

    // ---------- счётчик заблокированной рекламы «всего» ----------
    //
    // Ядро считает только за текущий сеанс (обнуляется при каждом новом
    // подключении) — здесь копим разницу в постоянные настройки, чтобы число
    // не терялось между подключениями и не зависело от того, открыт ли экран
    // приложения: poll работает, пока жива служба, а не только пока видна
    // активность.
    private var activeSettings: Settings? = null
    private var blockedSoFarInSession = 0

    /** Гашение уже идёт: второй раз ядро останавливать не надо. */
    @Volatile private var stopping = false

    /**
     * Идёт слив: для человека туннель уже выключен, но интерфейс VPN ещё
     * поднят и водит трафик напрямую. Отдельно от stopping: сливающаяся
     * служба ещё жива и может вернуться к работе по «Подключить».
     */
    @Volatile private var draining = false

    private val ticker = android.os.Handler(android.os.Looper.getMainLooper())

    // ---------- смена сети (Wi-Fi ↔ мобильная) ----------
    //
    // Раньше обрыв связи при переключении сети замечался только по внутреннему
    // таймеру проверки живости — заметная пауза перед тем, как туннель сам
    // сообразит переподключиться. ConnectivityManager сообщает о новой сети
    // сразу, и пул пересобирается, не дожидаясь таймера.
    private var connectivityManager: ConnectivityManager? = null
    private var networkCallback: ConnectivityManager.NetworkCallback? = null
    private var lastNetwork: Network? = null

    // ---------- Wi-Fi для сети устройств (ТЗ 11) ----------
    //
    // Отдельно от основной сети: дома без интернета Android уводит основную
    // сеть на мобильную, а соседи по сети устройств — в Wi-Fi. Сокет прямых
    // соединений привязывается к Wi-Fi (bindLAN), а при смене Wi-Fi клиент
    // сети пересоздаётся (mobile.LANChanged).
    @Volatile private var wifiNetwork: Network? = null
    private var wifiCallback: ConnectivityManager.NetworkCallback? = null

    /**
     * Без MulticastLock Android отбрасывает широковещательные пакеты, пока
     * экран выключен, — маяки соседей не доходят. Держим его только пока он
     * нужен: сервера нет, кого-то из знакомых устройств не видно и телефон в
     * Wi-Fi (см. updateMulticast).
     */
    private var multicastLock: WifiManager.MulticastLock? = null

    /** Экран включился — сервер ищется сразу, а не по таймеру. */
    private var screenReceiver: BroadcastReceiver? = null

    /** Последний текст уведомления: обновляем, только когда он сменился. */
    private var lastNotice = ""

    private fun accumulateBlockedTotal() {
        val s = activeSettings ?: return
        val blocked = try {
            JSONObject(statsJson).optInt("adsBlocked")
        } catch (e: Exception) {
            return
        }
        if (blocked > blockedSoFarInSession) {
            s.adBlockTotal += (blocked - blockedSoFarInSession).toLong()
            blockedSoFarInSession = blocked
        }
    }

    private fun registerNetworkCallback() {
        if (networkCallback != null) return
        val cm = getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager ?: return
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                val prev = lastNetwork
                lastNetwork = network
                updateMetered(cm)
                // Первый вызов — это просто исходная сеть, не смена. Кикать
                // пул, пока он ещё не поднят, тоже незачем — tunnel.kick()
                // сам по себе безвреден, но и толку от него в этот момент нет.
                if (prev != null && prev != network) {
                    onLog("сеть сменилась — пересобираю соединения")
                    try {
                        tunnel.networkChanged()
                    } catch (e: Exception) {
                        Log.w(TAG, "networkChanged: ${e.message}")
                    }
                }
            }

            // Та же сеть стала лимитной или перестала (точка доступа, отметка
            // «лимитная» в настройках Wi-Fi) — резервным копиям это важно.
            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
                updateMetered(cm)
            }
        }
        networkCallback = cb
        connectivityManager = cm
        updateMetered(cm)
        // registerDefaultNetworkCallback, а не registerNetworkCallback с общим
        // запросом: нужна именно смена сети, которую система считает основной
        // для исходящих соединений, а не появление любой сети в принципе
        // (второй Wi-Fi без интернета и подобное подняли бы лишний Kick).
        cm.registerDefaultNetworkCallback(cb)
        registerWifiCallback(cm)
        registerScreenReceiver()
    }

    private fun registerWifiCallback(cm: ConnectivityManager) {
        if (wifiCallback != null) return
        // Текущий Wi-Fi — сразу, до первого сокета: иначе первый вызов
        // onAvailable выглядел бы сменой сети и зря пересоздал бы клиента.
        @Suppress("DEPRECATION")
        wifiNetwork = cm.allNetworks.firstOrNull {
            cm.getNetworkCapabilities(it)?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true
        }
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                if (wifiNetwork == network) return
                wifiNetwork = network
                lanChanged("Wi-Fi появился")
            }

            override fun onLost(network: Network) {
                if (wifiNetwork != network) return
                wifiNetwork = null
                lanChanged("Wi-Fi пропал")
            }
        }
        wifiCallback = cb
        // Без требования «интернет проверен» (VALIDATED): домашний Wi-Fi с
        // вынутым кабелем нам как раз и нужен.
        val req = NetworkRequest.Builder().addTransportType(NetworkCapabilities.TRANSPORT_WIFI).build()
        try {
            cm.registerNetworkCallback(req, cb)
        } catch (e: Exception) {
            wifiCallback = null
            Log.w(TAG, "Wi-Fi: ${e.message}")
        }
    }

    private fun lanChanged(why: String) {
        onLog("сеть устройств: $why — пересобираю прямые соединения")
        // Остановка клиента ждёт его соединения — не в потоке системы.
        Thread {
            try {
                tunnel.lanChanged()
            } catch (e: Exception) {
                Log.w(TAG, "lanChanged: ${e.message}")
            }
        }.start()
        ticker.post { updateMulticast() }
    }

    private fun registerScreenReceiver() {
        if (screenReceiver != null) return
        val r = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) {
                // Сервер ищется в фоне с паузой до ~30 секунд; человек взял
                // телефон — пробуем сразу. Живую связь не трогаем.
                if (state != "reconnecting") return
                Thread {
                    try { tunnel.networkChanged() } catch (_: Exception) {}
                }.start()
            }
        }
        screenReceiver = r
        try {
            registerReceiver(r, IntentFilter(Intent.ACTION_SCREEN_ON))
        } catch (e: Exception) {
            screenReceiver = null
        }
    }

    /** MulticastLock — только пока нужен (см. multicastLock). */
    private fun updateMulticast() {
        val beacons = try { JSONObject(meshSummary).optBoolean("beacons") } catch (_: Exception) { false }
        val want = beacons && wifiNetwork != null && !stopping && !draining
        var lock = multicastLock
        if (want && lock == null) {
            val wm = applicationContext.getSystemService(Context.WIFI_SERVICE) as? WifiManager ?: return
            lock = wm.createMulticastLock("ssh_tunnel-mesh").apply { setReferenceCounted(false) }
            multicastLock = lock
        }
        try {
            if (want && lock?.isHeld == false) {
                lock.acquire()
                onLog("сеть устройств: ищу соседей маяками, в том числе при выключенном экране")
            } else if (!want && lock?.isHeld == true) {
                lock.release()
            }
        } catch (e: Exception) {
            Log.w(TAG, "MulticastLock: ${e.message}")
        }
    }

    private fun unregisterNetworkCallback() {
        wifiCallback?.let {
            wifiCallback = null
            try { connectivityManager?.unregisterNetworkCallback(it) } catch (_: Exception) {}
        }
        wifiNetwork = null
        screenReceiver?.let {
            screenReceiver = null
            try { unregisterReceiver(it) } catch (_: Exception) {}
        }
        try {
            multicastLock?.let { if (it.isHeld) it.release() }
        } catch (_: Exception) {}
        val cb = networkCallback ?: return
        networkCallback = null
        lastNetwork = null
        try {
            connectivityManager?.unregisterNetworkCallback(cb)
        } catch (e: Exception) {
            // Уже отписаны или служба системы недоступна — не критично.
        }
    }

    /**
     * Опрос состояния у ядра.
     *
     * События с состоянием приходят сами, но полагаться только на них нельзя:
     * одно потерянное сообщение — и экран навсегда застрял на «подключение…»,
     * хотя туннель работает. Опрос это исключает и заодно даёт живые счётчики.
     */
    private val poll = object : Runnable {
        override fun run() {
            val now = try {
                tunnel.state()
            } catch (e: Exception) {
                "error"
            }
            if (now != state) {
                report(now, detail)
            }
            statsJson = try { tunnel.statsJSON() } catch (e: Exception) { "{}" }
            meshSummary = try { tunnel.meshSummary() } catch (e: Exception) { """{"state":"off"}""" }
            exitJson = try { tunnel.exitJSON() } catch (e: Exception) { "" }
            serverBack(now)
            accumulateBlockedTotal()
            updateMulticast()
            refreshNotice()
            fireUpdate()
            ticker.postDelayed(this, 1500)
        }
    }

    /**
     * Сервер вернулся, а обход погас из-за его потери и человек его не
     * трогал — включить обход обратно (как круглая кнопка на ПК, ТЗ 20).
     */
    private fun serverBack(now: String) {
        if (!bypassAutoOff || now != "connected" || stopping || draining || bypassOn) return
        bypassAutoOff = false
        val mesh = meshOn
        val seq = humanSeq
        onLog("сервер снова на связи — обход блокировок включается обратно")
        Thread {
            val refused = try {
                synchronized(modesLock) {
                    // Человек успел нажать — его выбор уже применяется.
                    if (humanSeq != seq) return@Thread
                    tunnel.setModes(true, mesh)
                }
                null
            } catch (e: Exception) {
                e.message ?: "нет связи с сервером"
            }
            ticker.post {
                if (humanSeq != seq) return@post
                if (refused == null) bypassOn = true
                modesApplied(true, mesh, refused)
                // Сервер успел пропасть снова — ждём следующего возврата.
                if (refused != null) bypassAutoOff = true
            }
        }.start()
    }

    /** Лимитная ли сейчас связь — для «только по Wi-Fi» у резервных копий. */
    private fun updateMetered(cm: ConnectivityManager) {
        val metered = try { cm.isActiveNetworkMetered } catch (_: Exception) { false }
        try { Mobile.setMetered(metered) } catch (_: Exception) {}
    }

    override fun onCreate() {
        super.onCreate()
        current = this
        // Служба файлов и ключ SSH (ТЗ 4) живут в ядре, пока жив процесс.
        DeviceApps.init(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            // Второе «Отключить» подряд (кнопка уже показывает «выключен», но
            // слив ещё идёт) — просьба «да выключай уже».
            if (draining) finishDrain() else stopTunnel()
            return START_NOT_STICKY
        }
        if (intent?.action == ACTION_MODES) {
            // Нажал человек — его решение ядро больше не переписывает.
            bypassAutoOff = false
            humanSeq++
            val bypass = intent.getBooleanExtra(EXTRA_BYPASS, true)
            val mesh = intent.getBooleanExtra(EXTRA_MESH, true)
            val running = state != "stopped" && state != "error" && !draining
            if (!bypass && !(mesh && Settings(this).meshConfigured())) {
                if (running) stopTunnel()
                return START_NOT_STICKY
            }
            bypassOn = bypass
            meshOn = mesh
            if (running) {
                // В потоке: выключение сети устройств ждёт, пока её клиент
                // закроет соединения.
                Thread {
                    val refused = try {
                        synchronized(modesLock) { tunnel.setModes(bypass, mesh) }
                        null
                    } catch (e: Exception) {
                        e.message ?: "нет связи с сервером"
                    }
                    ticker.post { modesApplied(bypass, mesh, refused) }
                }.start()
                return START_STICKY
            }
            // Выключенный туннель — поднимаем с выбранными режимами, как и
            // обычное включение (в том числе возврат из слива).
        } else if (intent?.action == ACTION_START) {
            // Большая кнопка и запуск системой (always-on) включают всё.
            bypassOn = true
            meshOn = true
            bypassAutoOff = false
        }
        // Включили обратно, пока шёл слив: интерфейс VPN никуда не девался,
        // поэтому достаточно поднять пул. Сокеты приложений при этом не
        // рвутся вовсе — ни браузер, ни мессенджер ничего не заметят и
        // перезапускать их не придётся.
        if (draining && resumeFromDrain()) {
            return START_STICKY
        }
        // Всегда включённый VPN и система в целом могут попросить запуститься
        // ещё раз, пока туннель уже поднят или поднимается, — например, после
        // восстановления связи система заново подтверждает выбор этого
        // приложения как always-on VPN. Такой повторный запуск не должен рвать
        // уже работающее (или ещё поднимающееся) подключение.
        //
        // «Переподключение» — тоже поднятый туннель: сервер ищется в фоне (в
        // том числе когда туннель поднят без него ради сети устройств).
        if (state == "connected" || state == "connecting" || state == "reconnecting") {
            return START_STICKY
        }
        startTunnel()
        return START_STICKY
    }

    /**
     * Итог переключения кнопок. refused — обход не включился: туннель поднят
     * без сервера, а сервер так и не ответил (как на ПК — кнопка гаснет с
     * ошибкой, сеть устройств работает дальше).
     */
    private fun modesApplied(bypass: Boolean, mesh: Boolean, refused: String?) {
        if (refused != null) {
            bypassOn = false
            serverDown = refused
            serverDownKind = try { tunnel.serverDownKind() } catch (_: Exception) { "" }
            onLog("обход блокировок не включён: сервер недоступен ($refused)")
        } else {
            if (bypass) {
                serverDown = ""
                serverDownKind = ""
            }
            onLog(
                when {
                    bypass && mesh -> "обход блокировок и сеть устройств включены"
                    bypass -> "сеть устройств выключена — обход блокировок работает"
                    else -> "обход блокировок выключен — сайты идут напрямую, сеть устройств работает"
                }
            )
        }
        refreshNotice()
        fireUpdate()
    }

    private fun startTunnel() {
        val settings = Settings(this)
        if (!settings.ready()) {
            report("error", "не заполнены настройки")
            stopSelf()
            return
        }

        stopping = false
        serverDown = ""
        serverDownKind = ""
        activeSettings = settings
        blockedSoFarInSession = 0
        startForeground(NOTIFICATION_ID, notification("подключение…"))
        report("connecting", "")
        registerNetworkCallback()

        // В отдельном потоке: подключение к серверу и проверка его возможностей
        // занимают секунды, а держать всё это время главный поток нельзя.
        Thread {
            try {
                tunnel.setCallbacks(this)
                tunnel.configure(
                    settings.host,
                    settings.sshPort.toLong(),
                    settings.user,
                    settings.keyFile.absolutePath,
                    settings.knownHostsFile.absolutePath,
                    settings.poolSize.toLong(),
                    settings.directHosts,
                    settings.localViaTunnel,
                    settings.adBlockEnabled,
                    settings.adBlockListFile.absolutePath,
                    settings.adBlockAllowlist,
                    settings.udpRelayEnabled,
                )
                tunnel.setTunnelHosts(settings.tunnelHosts)
                // До StartCore — только запоминает кнопки, ошибок не бывает.
                try { tunnel.setModes(bypassOn, meshOn) } catch (_: Exception) {}
                val prof = settings.active()
                tunnel.configureMesh(
                    prof.meshEnabled && prof.meshKey.isNotBlank(),
                    prof.meshKey,
                    prof.meshName.ifBlank { android.os.Build.MODEL ?: "телефон" },
                    settings.deviceId,
                    prof.meshIncoming,
                    !prof.meshDirectOff,
                )
                // Другие серверы (ТЗ 20): дополнительные серверы сети
                // устройств с тем же ключом и цепочка выхода. Неверная
                // цепочка — ошибка подключения, а не тихий выход напрямую.
                tunnel.setProfiles(settings.coreProfilesJson(), prof.id, settings.udpRelayEnabled)
                tunnel.startCore()
                // Сервер не ответил, а сеть устройств нужна: ядро поднялось
                // без него (ТЗ 11) — обход выключен с ошибкой сервера, сеть
                // устройств работает, сервер ищется в фоне. Служба при этом
                // не останавливается.
                val down = tunnel.serverDownText()
                if (down.isNotBlank()) {
                    // Погас сам, без человека: вернётся вместе с сервером.
                    bypassAutoOff = bypassOn
                    bypassOn = false
                    serverDown = down
                    serverDownKind = tunnel.serverDownKind()
                    onLog("сервер недоступен ($down) — сеть устройств работает без него")
                }
                // «Выключить» нажали, пока подключались: дальше не идём —
                // иначе интерфейс VPN поднимется уже после остановки и
                // останется висеть без туннеля.
                if (stopping) { tunnel.stop(); return@Thread }

                // Сначала спрашиваем сервер, умеет ли он IPv6, и только потом
                // создаём интерфейс. Объявить телефону шестую версию и не суметь
                // её обслужить — худшее из возможного: приложения дружно уходят
                // туда, и не работает вообще ничего. Ровно это и случилось на
                // первой живой проверке.
                val hasIPv6 = tunnel.serverHasIPv6()
                onLog(
                    if (hasIPv6) "сервер умеет IPv6 — ведём его через туннель"
                    else "сервер без IPv6 — приложения пойдут по IPv4"
                )

                if (stopping) { tunnel.stop(); return@Thread }
                val iface = buildInterface(settings, hasIPv6)
                if (stopping) {
                    try { iface.close() } catch (e: Exception) {}
                    tunnel.stop()
                    return@Thread
                }
                fd = iface
                // detachFd, а не getFd: дальше дескриптором распоряжается Go и
                // закрывает его сам. Иначе его закрыли бы дважды.
                tunnel.startStack(iface.detachFd().toLong(), MTU.toLong())

                ticker.post { if (!stopping) ticker.post(poll) }
            } catch (e: Exception) {
                // Нажали «стоп», не дождавшись подключения, — это не ошибка, и
                // красной надписи здесь быть не должно.
                if (stopping) return@Thread
                val why = e.message ?: "не удалось запустить туннель"
                ticker.post { stopTunnel("error", why) }
            }
        }.start()
    }

    /**
     * Собирает виртуальный интерфейс.
     *
     * Ключевых мест два. Первое — маршруты: локальные сети в туннель не
     * заводятся вовсе, поэтому домашние сервисы остаются доступными без единой
     * проверки в коде. Второе — отбор приложений: за него отвечает сама
     * система, и это надёжнее любых наших правил.
     */
    private fun buildInterface(settings: Settings, withIPv6: Boolean): ParcelFileDescriptor {
        val builder = Builder()
            .setSession(getString(R.string.app_name))
            .setMtu(MTU)
            // Адрес самого интерфейса. Из того же диапазона, что и подставные
            // адреса, — настоящих сайтов там нет.
            .addAddress("198.18.0.1", 15)
            // DNS-сервер — отдельный адрес, не адрес самого интерфейса:
            // пакет на собственный адрес система может обработать сама и в
            // туннель не отдать. Тогда имена не разрешаются вообще.
            .addDnsServer(Mobile.dnsAddr())

        // Весь трафик — в туннель. Дальше из него вычитается локальная сеть.
        builder.addRoute("0.0.0.0", 0)

        // IPv6 объявляем ровно тогда, когда сервер умеет его обслужить.
        //
        // Если не объявлять, Android сам блокирует шестую версию и приложения
        // выбирают четвёртую — она через туннель работает. Если объявить, не
        // умея, получается наоборот: приложения уходят в IPv6 и упираются в
        // «open failed» на каждом соединении.
        if (withIPv6) {
            builder.addAddress("fd00:c0de:7075::1", 64)
            builder.addRoute("::", 0)
        }

        if (!settings.localViaTunnel) {
            excludeLocalNetworks(builder)
        }

        applyAppFilter(builder, settings)

        builder.setConfigureIntent(
            PendingIntent.getActivity(
                this, 0, Intent(this, MainActivity::class.java),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
        )

        return builder.establish()
            ?: throw IllegalStateException("система не выдала интерфейс")
    }

    /**
     * Убирает локальные сети из туннеля.
     *
     * Начиная с Android 13 для этого есть готовый способ. На более старых
     * версиях приходится перечислять маршруты вручную, поэтому там локальная
     * сеть остаётся в туннеле — иначе список маршрутов пришлось бы считать как
     * дополнение диапазонов, и одна ошибка в нём отрезала бы связь целиком.
     */
    private fun excludeLocalNetworks(builder: Builder) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) {
            Log.i(TAG, "Android старее 13: локальная сеть останется в туннеле")
            return
        }
        val local = listOf(
            "10.0.0.0" to 8,
            "172.16.0.0" to 12,
            "192.168.0.0" to 16,
            "169.254.0.0" to 16, // адреса без DHCP
            "100.64.0.0" to 10,  // сети mesh-VPN
        )
        for ((addr, prefix) in local) {
            try {
                builder.excludeRoute(android.net.IpPrefix(InetAddress.getByName(addr), prefix))
            } catch (e: Exception) {
                Log.w(TAG, "не удалось исключить $addr/$prefix: ${e.message}")
            }
        }
    }

    /**
     * Отбор приложений средствами системы.
     *
     * Смешивать разрешённые и запрещённые в одном подключении нельзя, поэтому
     * режимы взаимно исключающие — те же три, что и в настройках на компьютере.
     * Себя в туннель не заводим никогда: соединение с сервером должно идти мимо.
     */
    private fun applyAppFilter(builder: Builder, settings: Settings) {
        val apps = settings.filterApps
        when (settings.filterMode) {
            "only" -> {
                if (apps.isEmpty()) return
                for (pkg in apps) {
                    try {
                        builder.addAllowedApplication(pkg)
                    } catch (e: Exception) {
                        Log.w(TAG, "приложение $pkg не найдено")
                    }
                }
            }
            "except" -> {
                for (pkg in apps) {
                    try {
                        builder.addDisallowedApplication(pkg)
                    } catch (e: Exception) {
                        Log.w(TAG, "приложение $pkg не найдено")
                    }
                }
                excludeSelf(builder)
            }
            else -> excludeSelf(builder)
        }
    }

    private fun excludeSelf(builder: Builder) {
        try {
            builder.addDisallowedApplication(packageName)
        } catch (e: Exception) {
            Log.w(TAG, "не удалось исключить себя: ${e.message}")
        }
    }

    /**
     * Останавливает туннель, не занимая главный поток.
     *
     * Разделено надвое намеренно. Всё, что видит человек — надпись, цвет
     * кнопки, уведомление, — делается сразу. А вот погасить ядро может занять
     * секунды: оно ждёт свои фоновые задачи, и одна из них в этот момент
     * пытается достучаться до сервера. Именно так это и ломалось: при плохой
     * связи нажатие на кнопку останавливало главный поток целиком, и
     * приложение выглядело зависшим — жёлтый круг, который не реагирует.
     *
     * finalState нужен для несостоявшегося подключения: туннель гасится так же,
     * но на экране должно остаться «ошибка» и её причина. Раньше причина
     * затиралась сразу же, и человек видел просто «выключен» — без объяснения,
     * почему.
     */
    private fun stopTunnel(finalState: String = "stopped", finalDetail: String = "") {
        ticker.removeCallbacks(poll)
        unregisterNetworkCallback()
        statsJson = "{}"
        meshSummary = """{"state":"off"}"""
        exitJson = ""
        bypassAutoOff = false
        serverDown = ""
        serverDownKind = ""
        lastNotice = ""
        report(finalState, finalDetail)
        stopForeground(STOP_FOREGROUND_REMOVE)

        // onDestroy вызывает нас повторно после stopSelf — второй раз гасить
        // нечего.
        if (stopping || draining) return

        // Обычное «Отключить» — со сливом: связь с сервером рвём сейчас же, а
        // интерфейс VPN оставляем ещё на DRAIN_SECONDS, водя трафик напрямую.
        // Закрытие интерфейса рвёт разом все сокеты всех приложений, и узнают
        // они об этом не сразу — отсюда и берётся «выключил VPN, интернет
        // пропал, помогает перезапуск браузера».
        //
        // После аварии сливать нечего: человек не просил водить трафик мимо
        // сервера, он просил подключиться, и это не удалось.
        if (finalState == "stopped" && fd != null) {
            startDrain()
            return
        }
        hardStop()
    }

    /** Разрыв с сервером сейчас, закрытие интерфейса — через DRAIN_SECONDS. */
    private fun startDrain() {
        draining = true
        Thread {
            try {
                tunnel.drain(DRAIN_SECONDS.toLong())
            } catch (e: Exception) {
                Log.w(TAG, "слив: ${e.message}")
                ticker.post { finishDrain() }
                return@Thread
            }
        }.start()
        ticker.postDelayed(drainDeadline, DRAIN_SECONDS * 1000L)
    }

    private val drainDeadline = Runnable { finishDrain() }

    /** Конец слива — по таймеру, по второму «Отключить» или перед смертью. */
    private fun finishDrain() {
        if (!draining) return
        draining = false
        ticker.removeCallbacks(drainDeadline)
        hardStop()
    }

    /**
     * Включили обратно во время слива. Интерфейс VPN поднят и всё это время
     * никуда не девался, поэтому достаточно поднять пул: открытые сокеты
     * приложений продолжат работать как ни в чём не бывало, просто снова
     * через сервер.
     *
     * false означает «так не вышло» — тогда вызывающий запускает всё заново
     * обычным путём.
     */
    private fun resumeFromDrain(): Boolean {
        if (!draining || fd == null) return false
        draining = false
        ticker.removeCallbacks(drainDeadline)

        startForeground(NOTIFICATION_ID, notification("подключение…"))
        report("connecting", "")
        registerNetworkCallback()
        // Настройки могли поменяться, пока шёл слив, — а ядро и стек те же.
        // Правила маршрутизации применяем свежие, иначе «Сохранить» и
        // переподключение в эти секунды молча оставили бы старый список.
        val settings = Settings(this)
        activeSettings = settings
        Thread {
            try {
                tunnel.updateRouting(settings.directHosts, settings.localViaTunnel)
                tunnel.setTunnelHosts(settings.tunnelHosts)
                try { tunnel.setModes(bypassOn, meshOn) } catch (_: Exception) { bypassOn = false }
                tunnel.resume()
                ticker.post(poll)
            } catch (e: Exception) {
                // Сервер за это время пропал. Поднимать всё заново прямо
                // отсюда не станем: интерфейс уже держит старое ядро, и две
                // попытки наперегонки — верный способ получить два. Человек
                // увидит ошибку и нажмёт «Подключить» сам.
                Log.w(TAG, "возобновление: ${e.message}")
                ticker.post { stopTunnel("error", e.message ?: "не удалось включить обратно") }
            }
        }.start()
        return true
    }

    /** Настоящая остановка: гасим ядро, закрываем интерфейс, умираем. */
    private fun hardStop() {
        val iface = fd
        fd = null
        if (stopping) return
        stopping = true

        // Остановка ядра на плохой сети может затянуться (рвутся мёртвые
        // соединения). Ждём её не дольше STOP_WAIT_MS, дальше закрываем
        // интерфейс и завершаемся всё равно: ядро уже отвязано (core = nil) и
        // новому запуску не помешает. Иначе кнопка «висела» до перезапуска.
        Thread {
            val core = Thread {
                try {
                    tunnel.stop()
                } catch (e: Exception) {
                    Log.w(TAG, "остановка: ${e.message}")
                }
            }
            core.start()
            core.join(STOP_WAIT_MS)
            if (core.isAlive) Log.w(TAG, "остановка ядра затянулась — закрываем без неё")
            try {
                iface?.close()
            } catch (e: Exception) {
                // Дескриптор уже отдан в Go и закрыт там — это нормально.
            }
            ticker.post { stopSelf() }
        }.start()
    }

    override fun onDestroy() {
        current = null
        // Служба умирает — донашивать соединения будет некому.
        draining = false
        ticker.removeCallbacks(drainDeadline)
        stopTunnel()
        super.onDestroy()
    }

    override fun onRevoke() {
        // Систему попросили отдать VPN другому приложению: интерфейс у нас
        // отбирают, и оставлять его «на слив» невозможно.
        draining = false
        ticker.removeCallbacks(drainDeadline)
        stopTunnel()
        super.onRevoke()
    }

    // ---------- то, что ядро просит сделать у приложения ----------

    /**
     * Помечает сокет как идущий мимо туннеля.
     *
     * Без этого соединение с сервером ушло бы в собственный туннель и
     * зациклилось: система заворачивает внутрь весь трафик, включая наш.
     */
    override fun protect(fd: Long): Boolean = protect(fd.toInt())

    /**
     * Сокет сети устройств: мимо туннеля и к Wi-Fi, если он есть (см.
     * wifiNetwork). Нет Wi-Fi — только мимо туннеля, основной сетью.
     */
    override fun bindLAN(fd: Long): Boolean {
        val ok = protect(fd.toInt())
        val wifi = wifiNetwork ?: return ok
        try {
            // fromFd делает копию дескриптора: привязка — свойство самого
            // сокета, копию закрываем, сокет остаётся у Go.
            ParcelFileDescriptor.fromFd(fd.toInt()).use { wifi.bindSocket(it.fileDescriptor) }
        } catch (e: Exception) {
            Log.w(TAG, "привязка к Wi-Fi: ${e.message}")
        }
        return ok
    }

    override fun onState(state: String, detail: String, errorKind: String) {
        // После «Выключить» ядро ещё может прислать «переподключение» —
        // для человека туннель уже выключен, кнопку обратно не красим.
        if (stopping || draining) return
        report(state, connErrorText(errorKind, detail))
    }

    override fun onLog(line: String) {
        synchronized(log) {
            log.addLast(line)
            while (log.size > 200) log.removeFirst()
        }
        onApiLog?.invoke(line)
        fireUpdate()
    }

    /**
     * Разрешает имя средствами системы, минуя туннель.
     *
     * Своими силами ядро этого на телефоне не может: файла с настройками DNS,
     * который читают обычные программы, в Android нет.
     */
    override fun resolveLocal(name: String): String = try {
        InetAddress.getAllByName(name).joinToString(",") { it.hostAddress ?: "" }
    } catch (e: Exception) {
        ""
    }

    private fun fireUpdate() {
        onUpdate?.invoke()
        onApiUpdate?.invoke()
    }

    private fun report(newState: String, newDetail: String) {
        state = newState
        detail = newDetail
        fireUpdate()
        if (newState != "stopped") {
            lastNotice = notice()
            notificationManager().notify(NOTIFICATION_ID, notification(lastNotice))
        }
    }

    /** Уведомление — заново, только если текст сменился (опрос раз в 1,5 с). */
    private fun refreshNotice() {
        if (state == "stopped" || stopping || draining) return
        val text = notice()
        if (text == lastNotice) return
        lastNotice = text
        notificationManager().notify(NOTIFICATION_ID, notification(text))
    }

    /**
     * Текст уведомления. Обход включён — как раньше (состояние связи). Только
     * сеть устройств — как она живёт: через сервер / без сервера, напрямую /
     * нет связи и соседей не видно.
     */
    private fun notice(): String {
        if (bypassOn || !meshOn) return describe(state, detail)
        val m = try { JSONObject(meshSummary) } catch (_: Exception) { JSONObject() }
        return when (m.optString("state")) {
            "online" -> getString(R.string.mesh_notice_online)
            "local" -> getString(R.string.mesh_notice_local, m.optInt("direct"))
            "error" -> getString(R.string.mesh_notice_none)
            else -> describe(state, if (state == "reconnecting") "" else detail)
        }
    }

    /**
     * Перевод типовой ошибки подключения (ТЗ-13). errorKind разобран один
     * раз в общем коде на Go (internal/tunnel.ConnErrorKind) — здесь только
     * выбор текста под язык интерфейса из строковых ресурсов. Пустой код
     * (не ошибка, либо смена ключа сервера — своя отдельная защита) и
     * незнакомый код оставляют detail как есть.
     */
    private fun connErrorText(errorKind: String, detail: String): String = when (errorKind) {
        "auth" -> getString(R.string.conn_err_auth)
        "no_response" -> getString(R.string.conn_err_no_response)
        "refused" -> getString(R.string.conn_err_refused)
        "dropped" -> getString(R.string.conn_err_dropped)
        "other" -> getString(R.string.conn_err_other_prefix, detail)
        else -> detail
    }

    private fun describe(state: String, detail: String): String {
        val text = when (state) {
            "connected" -> "подключено"
            "connecting" -> "подключение…"
            "error" -> "ошибка"
            else -> state
        }
        return if (detail.isBlank()) text else "$text: $detail"
    }

    private fun notificationManager(): NotificationManager =
        getSystemService(NotificationManager::class.java)

    private fun notification(text: String): Notification {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL, getString(R.string.app_name), NotificationManager.IMPORTANCE_LOW
            )
            notificationManager().createNotificationChannel(channel)
        }
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val stop = PendingIntent.getService(
            this, 1, Intent(this, TunnelService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val b = Notification.Builder(this, CHANNEL)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(text)
            .setSmallIcon(R.drawable.ic_notify)
            .setContentIntent(open)
            .setOngoing(true)
            .addAction(
                Notification.Action.Builder(
                    null as android.graphics.drawable.Icon?,
                    getString(R.string.stop),
                    stop,
                ).build()
            )
        // «Общий буфер» (ТЗ 4): кнопка отправить буфер одним нажатием. Окно
        // ClipReadActivity прозрачное: оно берёт фокус, читает буфер и
        // закрывается. Только когда плагин добавлен и включён; список кнопок
        // обновляется вместе с уведомлением (при смене состояния туннеля).
        val clipOn = try { Mobile.clipActive() } catch (_: Throwable) { false }
        if (clipOn) {
            val send = PendingIntent.getActivity(
                this, 2, Intent(this, ClipReadActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            b.addAction(
                Notification.Action.Builder(
                    null as android.graphics.drawable.Icon?,
                    getString(R.string.clip_send_now),
                    send,
                ).build()
            )
        }
        return b.build()
    }
}
