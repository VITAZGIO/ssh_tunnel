package lanscan

import "testing"

// Вывод netsh на русской Windows 11 (подписи на языке системы).
const netshRU = `
Имя интерфейса : Беспроводная сеть
В настоящее время видно 2 сетей.

SSID 1 : Home
    Тип сети                : Инфраструктура
    Проверка подлинности    : WPA2-Personal
    Шифрование              : CCMP
    BSSID 1                 : aa:bb:cc:dd:ee:01
         Сигнал             : 80%
         Тип радио          : 802.11ax
         Диапазон           : 5 ГГц
         Канал              : 36
         Подключенные станции:        3
         Основные скорости (Мбит/с) : 6 12 24
    BSSID 2                 : aa:bb:cc:dd:ee:02
         Сигнал             : 40%
         Тип радио          : 802.11n
         Диапазон           : 2,4 ГГц
         Канал              : 6

SSID 2 :
    Тип сети                : Инфраструктура
    Проверка подлинности    : Открыть
    BSSID 1                 : 11:22:33:44:55:66
         Сигнал             : 10%
         Канал              : 11
`

func TestParseNetshNetworks(t *testing.T) {
	n := parseNetshNetworks(netshRU)
	if len(n) != 3 {
		t.Fatalf("сетей %d, ждал 3: %+v", len(n), n)
	}
	if n[0].SSID != "Home" || n[0].Channel != 36 || n[0].Freq != 5180 || n[0].Level != -60 || n[0].Sec != "WPA2-Personal" || n[0].Radio != "802.11ax" {
		t.Errorf("первая: %+v", n[0])
	}
	if n[1].Channel != 6 || n[1].Freq != 2437 {
		t.Errorf("вторая: %+v", n[1])
	}
	if n[2].SSID != "" || n[2].Channel != 11 || n[2].Freq != 2462 {
		t.Errorf("скрытая: %+v", n[2])
	}
}
