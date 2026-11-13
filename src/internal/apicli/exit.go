package apicli

// Команда «exit» — цепочка выхода (ТЗ 8): через какие серверы выходить в
// интернет.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// profileRef — сервер из GET /api/config.
type profileRef struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	ExitVia []string `json:"exitVia"`
}

func (r *runner) profiles() ([]profileRef, string, error) {
	b, _, err := r.c.Do("GET", "/api/config", nil, nil)
	if err != nil {
		return nil, "", err
	}
	var cfg struct {
		Profiles      []profileRef `json:"profiles"`
		ActiveProfile string       `json:"activeProfile"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, "", err
	}
	return cfg.Profiles, cfg.ActiveProfile, nil
}

// findProfile — сервер по id или имени (без учёта регистра).
func findProfile(ps []profileRef, s string) (profileRef, error) {
	for _, p := range ps {
		if p.ID == s {
			return p, nil
		}
	}
	var found []profileRef
	for _, p := range ps {
		if strings.EqualFold(p.Name, s) {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return profileRef{}, fmt.Errorf("нет сервера %q", s)
	}
	return profileRef{}, fmt.Errorf("серверов с именем %q несколько — укажи id", s)
}

// exitCmd: `exit` — цепочки всех серверов; `exit <сервер> <через> [<через>]`
// — задать; `exit <сервер> off` — убрать.
func (r *runner) exitCmd(a []string) (int, error) {
	ps, active, err := r.profiles()
	if err != nil {
		return exitError, err
	}
	if len(a) == 0 {
		names := map[string]string{}
		for _, p := range ps {
			names[p.ID] = p.Name
		}
		for _, p := range ps {
			path := []string{p.Name}
			for _, id := range p.ExitVia {
				n := names[id]
				if n == "" {
					n = "(удалён)"
				}
				path = append(path, n)
			}
			mark := " "
			if p.ID == active {
				mark = "*"
			}
			fmt.Fprintf(r.out, "%s %s\n", mark, strings.Join(path, " → "))
		}
		return exitOK, nil
	}
	p, err := findProfile(ps, a[0])
	if err != nil {
		return exitUsage, err
	}
	via := []string{}
	if !(len(a) == 2 && (a[1] == "off" || a[1] == "-")) {
		for _, s := range a[1:] {
			h, err := findProfile(ps, s)
			if err != nil {
				return exitUsage, err
			}
			via = append(via, h.ID)
		}
	}
	if len(a) == 1 {
		return exitUsage, errors.New("exit <сервер> <через> [<через>] или exit <сервер> off")
	}
	return r.call("POST", "/api/profile/exit", map[string]any{"id": p.ID, "via": via})
}
