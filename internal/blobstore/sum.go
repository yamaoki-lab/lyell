package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Sum は blob の中身の SHA-256 で, blob を呼ぶ名前になる. 同じ名前なら中身も同じとみなす. 32バイトの値なので, 置き場の外を指すような名前はそもそも作れない.
type Sum [sha256.Size]byte

// ParseSum は, 16進の64文字から Sum を作る.
func ParseSum(s string) (Sum, error) {
	var sum Sum
	if len(s) != hex.EncodedLen(len(sum)) {
		return Sum{}, fmt.Errorf("SHA-256 の16進は%d文字だが, %d文字だった", hex.EncodedLen(len(sum)), len(s))
	}
	if _, err := hex.Decode(sum[:], []byte(s)); err != nil {
		return Sum{}, fmt.Errorf("SHA-256 の16進として読めない: %w", err)
	}
	return sum, nil
}

// String は, 16進の64文字 (小文字) を返す.
func (s Sum) String() string {
	return hex.EncodeToString(s[:])
}
