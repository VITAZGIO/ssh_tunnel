package webui

// ТЗ 5: Расход трафика, Сканер сети, Общий буфер обмена. Пакеты приложений
// подключает internal/addons/devapps; здесь — только как окну ПК говорить с
// человеком (devnet.Warn → журнал).

func (n appNet) Warn(text string) {
	if n.a != nil && n.a.Bus != nil {
		n.a.Bus.Warnf("%s", text)
	}
}
