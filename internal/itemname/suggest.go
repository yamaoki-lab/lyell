package itemname

import (
	"strings"

	"github.com/clipperhouse/uax29/v2/graphemes"
)

// Suggest は, name のすべての規則違反を直した候補を返す. 違反がなければ, Check の自動修正後の名前を返す. 候補を作れない場合は "", false を返す.
func Suggest(name string) (string, bool) {
	name, violations := Check(name)
	seen := make(map[string]bool)
	for range 1024 {
		if len(violations) == 0 {
			return name, true
		}
		if name == "" || seen[name] {
			return "", false
		}
		seen[name] = true

		name = strings.Map(func(r rune) rune {
			switch {
			case isBidiControl(r):
				return -1
			case isForbidden(r):
				return '_'
			default:
				return r
			}
		}, name)
		// 末尾のピリオドを先に直すと, CON. はこの時点で CON_ になり, 予約名の修正が要らなくなる.
		if hasTrailingPeriod(name) {
			name = name[:len(name)-1] + "_"
		}
		if _, ok := reserved(name); ok {
			base, _, _ := strings.Cut(name, ".")
			name = base + "_" + name[len(base):]
		}
		name = shorten(name)
		if name == "" {
			return "", false
		}
		name, violations = Check(name)
	}
	return "", false
}

// shorten は, 長すぎる名前を, 拡張子を残して書記素クラスタの境界で短くする. 書記素クラスタを1つも残せない場合は空の名前を返す.
func shorten(name string) string {
	if nfc, nfd := nameByteLengths(name); max(nfc, nfd) <= MaxBytes {
		return name
	}
	stem, ext := name, ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		stem, ext = name[:dot], name[dot:]
	}

	// NFC で収まる境界だけを集め, 長い方から NFD でも収まるかを調べる.
	var ends []int
	limit := MaxBytes - len(ext)
	clusters := graphemes.FromString(stem)
	for clusters.Next() && clusters.End() <= limit {
		ends = append(ends, clusters.End())
	}
	for i := len(ends) - 1; i >= 0; i-- {
		candidate := stem[:ends[i]] + ext
		if _, nfd := nameByteLengths(candidate); nfd <= MaxBytes {
			return candidate
		}
	}
	return ""
}
