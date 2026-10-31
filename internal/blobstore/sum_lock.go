package blobstore

import (
	"context"
	"sync"
)

// sumLocks は, blob の名前 (Sum) ごとの排他制御. 同じ名前のロックは同時に1つしか取得できないが, 異なる名前どうしは互いに待たない.
type sumLocks struct {
	mu      sync.Mutex
	entries map[Sum]*sumLockEntry
}

type sumLockEntry struct {
	token chan struct{} // 容量1. 値が入っている間は, ロックが取得されている
	refs  int           // ロックを保持している1つと, 待機している数の合計. 0 になったら entries から削除する
}

// lock は, sum のロックを取得できるまで待機し, ロックを解放する関数を返す. 待機中に ctx が終了したら, 待機をやめて ctx のエラーを返す.
func (l *sumLocks) lock(ctx context.Context, sum Sum) (unlock func(), err error) {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[Sum]*sumLockEntry)
	}
	e, ok := l.entries[sum]
	if !ok {
		e = &sumLockEntry{token: make(chan struct{}, 1)}
		l.entries[sum] = e
	}
	e.refs++
	l.mu.Unlock()

	select {
	case e.token <- struct{}{}:
		return func() {
			<-e.token
			l.release(sum, e)
		}, nil
	case <-ctx.Done():
		l.release(sum, e)
		return nil, ctx.Err()
	}
}

func (l *sumLocks) release(sum Sum, e *sumLockEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(l.entries, sum)
	}
}
