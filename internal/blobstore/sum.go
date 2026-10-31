package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Sum は, blob の中身の SHA-256 で, blob の名前として使用する. 名前が同じなら中身も同じとみなす. 32バイトの値なので, 保存先の外を指す名前は構造上作成できない.
type Sum [sha256.Size]byte

// ParseSum は, 16進数64文字の表記から Sum を生成する.
func ParseSum(s string) (Sum, error) {
	var sum Sum
	if len(s) != hex.EncodedLen(len(sum)) {
		return Sum{}, fmt.Errorf("SHA-256 の16進数表記は%d文字だが, %d文字だった", hex.EncodedLen(len(sum)), len(s))
	}
	if _, err := hex.Decode(sum[:], []byte(s)); err != nil {
		return Sum{}, fmt.Errorf("SHA-256 の16進数表記として解釈できない: %w", err)
	}
	return sum, nil
}

// String は, 16進数64文字 (小文字) の表記を返す.
func (s Sum) String() string {
	return hex.EncodeToString(s[:])
}
