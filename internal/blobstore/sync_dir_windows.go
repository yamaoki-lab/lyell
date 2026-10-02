package blobstore

// syncDir は, Windows では何もしない. Windows ではディレクトリを fsync できない (FlushFileBuffers がディレクトリに対してはエラーになる). NTFS はディレクトリの項目の変更をジャーナルに記録するので, 停電の後に壊れた状態にはならないが, 直前の rename が残ることまでは保証されない.
func syncDir(string) error {
	return nil
}
