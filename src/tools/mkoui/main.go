// mkoui собирает базу производителей по MAC для «Сканера сети» из
// oui.csv IEEE (https://standards-oui.ieee.org/oui/oui.csv):
//
//	go run ./tools/mkoui oui.csv internal/lanscan/oui.txt.gz
//
// Берутся только блоки MA-L (первые 3 байта MAC), имена укорачиваются
// («Espressif Inc.» → «Espressif»): в списке нужно узнать производителя, а не
// юридическое лицо.
package main

import (
	"compress/gzip"
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var tail = regexp.MustCompile(`(?i)[,.]?\s+(co\.?,?\s*ltd\.?|corporation|corp\.?|inc\.?|incorporated|limited|ltd\.?|llc|gmbh|s\.?a\.?|a\.?g\.?|b\.?v\.?|oy|ab|technologies|technology|electronics|international|communications?)$`)

func short(n string) string {
	n = strings.Join(strings.Fields(n), " ")
	for {
		m := tail.ReplaceAllString(n, "")
		m = strings.TrimRight(m, " ,.")
		if m == n || m == "" {
			return n
		}
		n = m
	}
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "mkoui oui.csv out.txt.gz")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		panic(err)
	}
	var lines []string
	for _, r := range rows {
		if len(r) < 3 || r[0] != "MA-L" || len(r[1]) != 6 {
			continue
		}
		lines = append(lines, strings.ToUpper(r[1])+"\t"+short(r[2]))
	}
	sort.Strings(lines)
	out, err := os.Create(os.Args[2])
	if err != nil {
		panic(err)
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	zw.Write([]byte(strings.Join(lines, "\n") + "\n"))
	zw.Close()
	out.Close()
	fmt.Println(len(lines), "записей")
}
