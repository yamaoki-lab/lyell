package blobstore

// syncDir は, Windows では何もしない. Windows ではディレクトリを fsync できない (ディレクトリに対する FlushFileBuffers はエラーになる). NTFS はディレクトリのエントリの変更をジャーナルに記録するので, 停電の後に不整合な状態にはならないが, 直前の rename が永続化されていることまでは保証されない.
func syncDir(string) error {
	return nil
}
