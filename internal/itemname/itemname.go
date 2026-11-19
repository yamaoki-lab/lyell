// Package itemname は, revision の名前を自動で修正し, 名前の規則を満たすかを確かめる.
//
// 名前はエクスポートやマウントの際にそのままファイル名になる. そのため, どの一般的なファイルシステムでも変更なしに受け入れられる名前だけを許し, 見た目が同じ名前が同じ形で保存されるよう, 1つの正規化された形に揃える. 規則は, 最も厳しい Windows に合わせる.
package itemname

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// MaxBytes は, 名前の長さの上限 (UTF-8 のバイト数). UTF-8 のバイト数は UTF-16 のコード単位の数やコードポイントの数を下回らないので, NFC と NFD の両方でこの上限に収めれば, 一般的なファイルシステムの名前の長さの上限 (255 バイト, 255 UTF-16 コード単位など) に収まる.
const MaxBytes = 255

// Rule は, 名前の規則.
type Rule int

const (
	// RuleForbiddenCharacter は, Windows のファイル名に使えない文字 (< > : " / \ | ? *) を含まないこと.
	RuleForbiddenCharacter Rule = iota + 1
	// RuleBidiControl は, 双方向テキストの制御文字 (U+202A から U+202E, U+2066 から U+2069) を含まないこと. 表示される順序が変わり, 拡張子などを偽れるため.
	RuleBidiControl
	// RuleTrailingPeriod は, ピリオドで終わらないこと.
	RuleTrailingPeriod
	// RuleReserved は, "." と "..", および Windows の予約名 (拡張子が続く場合も含む) でないこと.
	RuleReserved
	// RuleTooLong は, NFC と NFD のどちらでも, UTF-8 で MaxBytes バイト以内であること.
	RuleTooLong
	// RuleBlank は, 空でない名前が, 自動修正の後に空にならないこと. 名前を付けようとしたクライアントが, 気づかないうちに名前なしにしてしまうのを防ぐため.
	RuleBlank
)

// Violation は, 名前の規則への違反.
type Violation struct {
	Rule Rule
	// Description は, 日本語の説明. 違反の原因となった文字などを含む.
	Description string
}

// Check は, name を自動で修正した名前と, 修正後の名前が満たさない規則の一覧を返す. 一覧は Rule の順に並べる. 空の name は名前なしを表すので, 空のまま違反なしで返す.
//
// 自動修正は, 見た目を変えない修正だけを行う. 修正後の名前に Check を再び適用しても, 名前は変わらない.
func Check(name string) (string, []Violation) {
	if name == "" {
		return "", nil
	}
	fixed := fix(name)
	if fixed == "" {
		return "", []Violation{{Rule: RuleBlank, Description: "自動修正の後に名前が空になります. 空白や制御文字だけの名前は使えません"}}
	}
	var vs []Violation
	if cs := matching(fixed, isForbidden); cs != nil {
		vs = append(vs, Violation{Rule: RuleForbiddenCharacter, Description: fmt.Sprintf("名前に使えない文字 %s を含みます", strings.Join(cs, ", "))})
	}
	if cs := matching(fixed, isBidiControl); cs != nil {
		vs = append(vs, Violation{Rule: RuleBidiControl, Description: fmt.Sprintf("双方向テキストの制御文字 %s を含みます", strings.Join(cs, ", "))})
	}
	if strings.HasSuffix(fixed, ".") {
		vs = append(vs, Violation{Rule: RuleTrailingPeriod, Description: "名前をピリオドで終えることはできません"})
	}
	if d, ok := reserved(fixed); ok {
		vs = append(vs, Violation{Rule: RuleReserved, Description: d})
	}
	if nfc, nfd := len(fixed), len(norm.NFD.String(fixed)); nfc > MaxBytes || nfd > MaxBytes {
		vs = append(vs, Violation{Rule: RuleTooLong, Description: fmt.Sprintf("名前は UTF-8 で %d バイト以内にしてください. NFC で %d バイト, NFD で %d バイトです", MaxBytes, nfc, nfd)})
	}
	return fixed, vs
}

// fix は, 見た目を変えない修正を, 次の順に行う.
//  1. ゼロ幅スペース (U+200B), BOM (U+FEFF), ソフトハイフン (U+00AD) を削除する.
//  2. 改行 (LF, CR, CRLF, U+2028, U+2029, NEL U+0085) とタブを, それぞれ1つのスペースに置き換える.
//  3. その他の C0・C1 制御文字と DEL を削除する.
//  4. 前後の White_Space を取り除く.
//  5. NFC に正規化する. ただし CJK 互換漢字はそのまま残す.
func fix(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		i += size
		switch {
		case r == '\u200B' || r == '\uFEFF' || r == '\u00AD':
		case r == '\r' && strings.HasPrefix(name[i:], "\n"):
			// CRLF は1つの改行として, 1つのスペースにする.
			i++
			b.WriteByte(' ')
		case r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029' || r == '\u0085' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7F || (0x80 <= r && r <= 0x9F):
		default:
			b.WriteRune(r)
		}
	}
	trimmed := strings.TrimFunc(b.String(), func(r rune) bool { return unicode.Is(unicode.White_Space, r) })
	return nfcKeepingCompatibilityIdeographs(trimmed)
}

// nfcKeepingCompatibilityIdeographs は, s を NFC に正規化する. ただし CJK 互換漢字は, 統合漢字に置き換えると字体が変わるので, そのまま残す.
// CJK 互換漢字は, 前後のどの文字とも合成されず, 結合文字の並べ替えの境界になる (正準結合クラスが 0) ので, その位置で区切ってそれぞれを NFC にしても, 残りの部分は全体を NFC にした場合と同じになる.
func nfcKeepingCompatibilityIdeographs(s string) string {
	var b strings.Builder
	start := 0
	for i, r := range s {
		if isCompatibilityIdeograph(r) {
			b.WriteString(norm.NFC.String(s[start:i]))
			b.WriteRune(r)
			start = i + utf8.RuneLen(r)
		}
	}
	b.WriteString(norm.NFC.String(s[start:]))
	return b.String()
}

// isCompatibilityIdeograph は, r が CJK 互換漢字 (U+F900 から U+FAFF, U+2F800 から U+2FA1F) かを返す.
func isCompatibilityIdeograph(r rune) bool {
	return (0xF900 <= r && r <= 0xFAFF) || (0x2F800 <= r && r <= 0x2FA1F)
}

func isForbidden(r rune) bool {
	return strings.ContainsRune(`<>:"/\|?*`, r)
}

func isBidiControl(r rune) bool {
	return (0x202A <= r && r <= 0x202E) || (0x2066 <= r && r <= 0x2069)
}

// matching は, s に含まれる文字のうち match を満たすものを, 初めて現れた順に重複なく, 説明に使う表記 (表示できる文字は「」で囲み, それ以外は U+XXXX) で返す. 無ければ nil を返す.
func matching(s string, match func(rune) bool) []string {
	var cs []string
	seen := map[rune]bool{}
	for _, r := range s {
		if !match(r) || seen[r] {
			continue
		}
		seen[r] = true
		if unicode.IsPrint(r) {
			cs = append(cs, "「"+string(r)+"」")
		} else {
			cs = append(cs, fmt.Sprintf("U+%04X", r))
		}
	}
	return cs
}

// reservedNames は, Windows の予約名. 大文字小文字を区別せずに比べる. COM と LPT には, 上付きの数字 (¹ ² ³) が続くものも含む.
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"COM¹": true, "COM²": true, "COM³": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"LPT¹": true, "LPT²": true, "LPT³": true,
}

// reserved は, name が "." か "..", または Windows の予約名なら, その説明と true を返す. 予約名は, NUL.txt や nul.tar.gz のように拡張子が続いても予約名として扱う.
func reserved(name string) (string, bool) {
	if name == "." || name == ".." {
		return fmt.Sprintf("「%s」は名前に使えません", name), true
	}
	base, _, _ := strings.Cut(name, ".")
	if reservedNames[asciiUpper(base)] {
		return fmt.Sprintf("「%s」は Windows の予約名のため使えません. 拡張子が続いても予約名になります", base), true
	}
	return "", false
}

// asciiUpper は, s の ASCII の英小文字だけを大文字にする. 大文字にすると予約名の英字になる文字は ASCII の英字の他に無いので, ASCII の範囲だけ変換すれば足りる.
func asciiUpper(s string) string {
	return strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, s)
}
