package itemstore

import (
	"fmt"
	"regexp"

	"google.golang.org/protobuf/proto"

	storagev1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/storage/v1alpha1"
)

// maxExtensionNameLen は, 拡張機能の名前の上限 (バイト).
const maxExtensionNameLen = 128

// maxExtensionDataLen は, revision の body の extension_data の上限 (バイト). それより大きい中身は, blob を持つ別の item にして入力へのリンクで参照する. 上限は後から互換性を壊さずに引き上げられるが, 引き下げられないので, 小さく始める.
const maxExtensionDataLen = 16 << 10

// extensionNamePattern は, 拡張機能の名前の形式. Protobuf のパッケージ名に倣い, "." で区切った2つ以上の部分からなり, 各部分は英小文字で始まり英小文字, 数字, "_" だけを含む (lyell.collection 等). 名前が衝突しないよう, 最初の部分には所有者の名前を置く. バージョンは名前に含めず, 必要なら拡張機能が自身のデータに持つ.
var extensionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

func validateExtensionName(name string) error {
	if len(name) > maxExtensionNameLen {
		return fmt.Errorf("%w: 拡張機能の名前が %d バイトです (%d バイト以下である必要があります)", ErrInvalid, len(name), maxExtensionNameLen)
	}
	if !extensionNamePattern.MatchString(name) {
		return fmt.Errorf("%w: 拡張機能の名前 %q の形式が不正です (lyell.collection のように, 英小文字で始まり英小文字, 数字, _ だけを含む部分を . で2つ以上つなげます)", ErrInvalid, name)
	}
	return nil
}

// validateItemExtension は, item の種類と拡張機能の名前の組み合わせを検証する. extension の item は名前を持ち, それ以外の item は持たない.
func validateItemExtension(kind Kind, extension string) error {
	if kind == KindExtension {
		// 空の名前も形式の検査で拒否される.
		return validateExtensionName(extension)
	}
	if extension != "" {
		return fmt.Errorf("%w: %s の item は拡張機能の名前 (%q) を持てません", ErrInvalid, kind, extension)
	}
	return nil
}

// validateContentForKind は, 中身が item の種類に合うかを検証する. extension の item の revision は blob を持たず, extension_data を持てる. それ以外の item の revision は blob を持ち, extension_data を持たない. requireBlob が false なら, blob を持たないことは許す (blob を送らずに検証する場合).
//
// 呼び出し元は先に Content.validate で Body が読めることを確かめておく.
func validateContentForKind(kind Kind, c Content, requireBlob bool) error {
	var b storagev1alpha1.RevisionBody
	_ = proto.Unmarshal(c.Body, &b) // Content.validate で読めることを確かめてある
	data := b.GetExtensionData()
	if kind == KindExtension {
		if c.hasBlob() {
			return fmt.Errorf("%w: %s の item の revision は blob を持てません", ErrInvalid, kind)
		}
		if len(data) > maxExtensionDataLen {
			return fmt.Errorf("%w: 拡張機能のデータが %d バイトです (%d バイト以下である必要があります)", ErrInvalid, len(data), maxExtensionDataLen)
		}
		return nil
	}
	if requireBlob && !c.hasBlob() {
		return fmt.Errorf("%w: %s の item の revision には blob が必要です", ErrInvalid, kind)
	}
	if len(data) > 0 {
		return fmt.Errorf("%w: %s の item の revision は拡張機能のデータを持てません", ErrInvalid, kind)
	}
	return nil
}
