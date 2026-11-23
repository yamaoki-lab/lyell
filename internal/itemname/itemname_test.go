package itemname

import (
	"slices"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

func rules(vs []Violation) []Rule {
	var rs []Rule
	for _, v := range vs {
		rs = append(rs, v.Rule)
	}
	return rs
}

func TestUnicodeVersionsMatch(t *testing.T) {
	// 前後の空白の判定は Go の unicode を, NFC への正規化は x/text の norm を使う. Unicode の版が違うと, 一方だけが知る文字の扱いが食い違うので, Go を上げて版がずれたら x/text も上げる.
	if norm.Version != unicode.Version {
		t.Errorf("norm.Version = %s, unicode.Version = %s, want 同じ版", norm.Version, unicode.Version)
	}
}

func TestCheck_Fixes(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"ゼロ幅スペースを消す", "a\u200Bb", "ab"},
		{"BOM を消す", "\uFEFFab", "ab"},
		{"ソフトハイフンを消す", "a\u00ADb", "ab"},
		{"LF をスペースにする", "a\nb", "a b"},
		{"CR をスペースにする", "a\rb", "a b"},
		{"CRLF を1つのスペースにする", "a\r\nb", "a b"},
		{"LF CR は2つのスペースにする", "a\n\rb", "a  b"},
		{"U+2028 をスペースにする", "a\u2028b", "a b"},
		{"U+2029 をスペースにする", "a\u2029b", "a b"},
		{"NEL をスペースにする", "a\u0085b", "a b"},
		{"タブをスペースにする", "a\tb", "a b"},
		{"C0 制御文字を消す", "a\x00\x01\x1Fb", "ab"},
		{"DEL を消す", "a\x7Fb", "ab"},
		{"C1 制御文字を消す", "a\u0080\u009Fb", "ab"},
		{"前後の White_Space を取り除く", " \u3000\u00A0a b\u2003 ", "a b"},
		{"間の White_Space は残す", "a\u3000b", "a\u3000b"},
		{"前後の改行とタブも取り除く", "\t\na\r\n", "a"},
		{"NFC にする", "e\u0301", "\u00E9"},
		{"NFC にする (濁点)", "\u304B\u3099", "\u304C"},
		{"CJK 互換漢字は残す", "\uF900", "\uF900"},
		{"補助面の CJK 互換漢字は残す", "\U0002F800", "\U0002F800"},
		{"CJK 互換漢字の前後は NFC にする", "e\u0301\uFA10e\u0301", "\u00E9\uFA10\u00E9"},
		{"CJK 互換漢字に続く結合文字は残す", "\uF900\u0301", "\uF900\u0301"},
		{"消した後に前後の空白を取り除く", "\u200B a \u200B", "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, vs := Check(tt.in)
			if got != tt.want || vs != nil {
				t.Errorf("Check(%+q) = %+q, %v, want %+q, nil", tt.in, got, rules(vs), tt.want)
			}
			// 修正後の名前を再び修正しても変わらない.
			if again, _ := Check(got); again != got {
				t.Errorf("Check(%+q) = %+q, want 変わらない", got, again)
			}
		})
	}
}

func TestCheck_Rejects(t *testing.T) {
	tests := []struct {
		name, in string
		want     []Rule
	}{
		{"<", "a<b", []Rule{RuleForbiddenCharacter}},
		{">", "a>b", []Rule{RuleForbiddenCharacter}},
		{":", "a:b", []Rule{RuleForbiddenCharacter}},
		{`"`, `a"b`, []Rule{RuleForbiddenCharacter}},
		{"/", "a/b", []Rule{RuleForbiddenCharacter}},
		{`\`, `a\b`, []Rule{RuleForbiddenCharacter}},
		{"|", "a|b", []Rule{RuleForbiddenCharacter}},
		{"?", "a?b", []Rule{RuleForbiddenCharacter}},
		{"*", "a*b", []Rule{RuleForbiddenCharacter}},
		{"U+202A", "a\u202Ab", []Rule{RuleBidiControl}},
		{"U+202E", "a\u202Eb", []Rule{RuleBidiControl}},
		{"U+2066", "a\u2066b", []Rule{RuleBidiControl}},
		{"U+2069", "a\u2069b", []Rule{RuleBidiControl}},
		{"末尾のピリオド", "a.", []Rule{RuleTrailingPeriod}},
		{"末尾のピリオドは空白を取り除いた後で調べる", "a. ", []Rule{RuleTrailingPeriod}},
		{".", ".", []Rule{RuleTrailingPeriod, RuleReserved}},
		{"..", "..", []Rule{RuleTrailingPeriod, RuleReserved}},
		{"CON", "CON", []Rule{RuleReserved}},
		{"PRN", "PRN", []Rule{RuleReserved}},
		{"AUX", "AUX", []Rule{RuleReserved}},
		{"NUL", "NUL", []Rule{RuleReserved}},
		{"小文字の nul", "nul", []Rule{RuleReserved}},
		{"大文字小文字が混ざった Nul", "Nul", []Rule{RuleReserved}},
		{"拡張子が続く NUL.txt", "NUL.txt", []Rule{RuleReserved}},
		{"拡張子が2つ続く nul.tar.gz", "nul.tar.gz", []Rule{RuleReserved}},
		{"COM1", "COM1", []Rule{RuleReserved}},
		{"COM9", "com9", []Rule{RuleReserved}},
		{"COM¹", "COM\u00B9", []Rule{RuleReserved}},
		{"COM³", "com\u00B3.log", []Rule{RuleReserved}},
		{"LPT1", "LPT1", []Rule{RuleReserved}},
		{"LPT9", "lpt9", []Rule{RuleReserved}},
		{"LPT²", "LPT\u00B2", []Rule{RuleReserved}},
		{"NFC で 256 バイト", strings.Repeat("a", 256), []Rule{RuleTooLong}},
		// U+00E9 は NFC で2バイト, NFD で3バイト (e と U+0301).
		{"NFD だけで 255 バイトを超える", strings.Repeat("\u00E9", 86), []Rule{RuleTooLong}},
		{"空白だけ", "   ", []Rule{RuleBlank}},
		{"消える文字だけ", "\u200B\t\x00", []Rule{RuleBlank}},
		{"複数の規則に違反する", "a:\u202Eb.", []Rule{RuleForbiddenCharacter, RuleBidiControl, RuleTrailingPeriod}},
		{"予約名で禁止文字を含む", "con.a?", []Rule{RuleForbiddenCharacter, RuleReserved}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, vs := Check(tt.in)
			if got := rules(vs); !slices.Equal(got, tt.want) {
				t.Errorf("Check(%+q) の違反 = %v, want %v", tt.in, got, tt.want)
			}
			for _, v := range vs {
				if v.Description == "" {
					t.Errorf("Check(%+q) の %v の説明が空", tt.in, v.Rule)
				}
			}
		})
	}
}

func TestCheck_Accepts(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"空 (名前なし)", ""},
		{"日本語", "撮影の記録"},
		{"拡張子付き", "photo.jpg"},
		{"ピリオドで始まる", ".gitignore"},
		{"大文字小文字だけが違う", "Photo.JPG"},
		{"予約名を含むが予約名ではない", "CONSOLE"},
		{"予約名の前に文字がある", "aNUL.txt"},
		{"COM0 は予約名ではない", "COM0"},
		{"未割り当てのコードポイント", "\U0002FFFD"},
		{"補助面の変体仮名", "\U0001B005"},
		{"ZWJ", "\U0001F468\u200D\U0001F469"},
		{"ZWNJ", "a\u200Cb"},
		{"異体字セレクタ", "\u845B\uFE00"},
		{"補助面の異体字セレクタ", "\u845B\U000E0100"},
		{"CJK 互換漢字", "\uF900"},
		{"NFC と NFD で 255 バイト", strings.Repeat("a", 255)},
		// U+00E9 は NFD で3バイトなので, 85 文字は NFD で 255 バイト.
		{"NFD でちょうど 255 バイト", strings.Repeat("\u00E9", 85)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, vs := Check(tt.in)
			if vs != nil {
				t.Errorf("Check(%+q) の違反 = %v, want なし", tt.in, rules(vs))
			}
			if got != tt.in {
				t.Errorf("Check(%+q) = %+q, want そのまま", tt.in, got)
			}
		})
	}
}

func TestCheck_TooLongCountsBothForms(t *testing.T) {
	// 前提の確認: テストに使う文字の NFC と NFD のバイト数.
	if nfc, nfd := len("\u00E9"), len(norm.NFD.String("\u00E9")); nfc != 2 || nfd != 3 {
		t.Fatalf("U+00E9 の NFC, NFD のバイト数 = %d, %d, want 2, 3", nfc, nfd)
	}
	_, vs := Check(strings.Repeat("\u00E9", 86))
	if len(vs) != 1 || !strings.Contains(vs[0].Description, "NFD で 258 バイト") {
		t.Errorf("違反 = %v, want NFD のバイト数を含む説明1件", vs)
	}
}

func TestCheck_DescriptionNamesCharacters(t *testing.T) {
	_, vs := Check("a:b?c:\u202Ed")
	if len(vs) != 2 {
		t.Fatalf("違反 = %v, want 2件", vs)
	}
	if d := vs[0].Description; !strings.Contains(d, "「:」, 「?」") || strings.Count(d, "「:」") != 1 {
		t.Errorf("禁止文字の説明 = %q, want 「:」, 「?」を1回ずつ含む", d)
	}
	if d := vs[1].Description; !strings.Contains(d, "U+202E") {
		t.Errorf("双方向テキストの制御文字の説明 = %q, want U+202E を含む", d)
	}
}

func TestSuggest(t *testing.T) {
	stem := strings.Repeat("a", 250)
	suffix := "." + strings.Repeat("a", 251)
	zwjStem := strings.Repeat("a", 247)
	ivsStem := strings.Repeat("a", 248)
	zwj := "👨\u200D👩"
	ivs := "葛\U000E0100"
	tests := []struct {
		name, in, want string
		ok             bool
	}{
		{"予約名の末尾に下線を足す", "CON", "CON_", true},
		{"予約名の最初のピリオドの前に下線を足す", "NUL.txt", "NUL_.txt", true},
		{"末尾のピリオドを下線にする", "メモ.", "メモ_", true},
		{"末尾のピリオドの最後の1つだけを下線にする", "メモ...", "メモ.._", true},
		{"ピリオド1つを下線にする", ".", "_", true},
		{"ピリオド2つのうち末尾だけを下線にする", "..", "._", true},
		{"末尾のピリオドを先に直して予約名の修正が不要になる", "CON.", "CON_", true},
		{"末尾のピリオドを直した後も予約名なら下線を足す", "NUL.txt.", "NUL_.txt_", true},
		{"禁止文字をそれぞれ下線にする", `a<>:"/\|?*b`, "a_________b", true},
		{"双方向テキストの制御文字を消す", "a\u202A\u202E\u2066\u2069b", "ab", true},
		{"最後のピリオドから始まる拡張子を残す", stem + "3号館の廊下 v1.2 final.ply", stem + "3.ply", true},
		{"先頭のピリオドだけなら拡張子なしとして短くする", "." + strings.Repeat("a", MaxBytes), "." + strings.Repeat("a", MaxBytes-1), true},
		{"拡張子のない長い名前を短くする", strings.Repeat("a", MaxBytes+1), strings.Repeat("a", MaxBytes), true},
		{"NFC で収まっても NFD で超える名前を短くする", strings.Repeat("é", 86), strings.Repeat("é", 85), true},
		{"ZWJ シーケンスの途中で切らない", zwjStem + zwj + ".ply", zwjStem + ".ply", true},
		{"異体字セレクタの直前で切らない", ivsStem + ivs + ".ply", ivsStem + ".ply", true},
		{"双方向テキストの制御文字を消した前後で文字を合成する", strings.Repeat("a", 252) + "e\u202E\u0301", strings.Repeat("a", 252) + "é", true},
		{"禁止文字と双方向テキストの制御文字と末尾のピリオドと予約名を直す", "NU\u202EL.a?.", "NUL_.a__", true},
		{"空白を取り除いて現れる予約名を次の反復で直す", "\u202E CON.txt", "CON_.txt", true},
		{"自動修正の後に空なら候補なし", " \u200B ", "", false},
		{"双方向テキストの制御文字を消して空なら候補なし", "\u202E", "", false},
		{"双方向テキストの制御文字を消して空白だけが残っても候補なし", "\u202E \u2066", "", false},
		{"拡張子の前に書記素クラスタを残せなければ候補なし", "a." + strings.Repeat("a", MaxBytes-1), "", false},
		{"拡張子の前の1文字で NFD の上限を超えれば候補なし", "é." + strings.Repeat("a", MaxBytes-3), "", false},
		{"予約名を直して短くすると同じ予約名に戻るなら候補なし", "CONx" + suffix, "", false},
		{"違反がなければ自動修正後の名前を返す", "\u200B e\u0301 ", "é", true},
		{"最初から名前なしなら空の名前を返す", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Suggest(tt.in)
			if got != tt.want || ok != tt.ok {
				t.Errorf("Suggest(%+q) = %+q, %t, want %+q, %t", tt.in, got, ok, tt.want, tt.ok)
			}
			if ok {
				if fixed, vs := Check(got); fixed != got || len(vs) != 0 {
					t.Errorf("Check(%+q) = %+q, %v, want 変更なし, 違反なし", got, fixed, rules(vs))
				}
			}
		})
	}
}
