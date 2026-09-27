package blobstore

import (
	"context"
	"sync"
)

// sumLocks は, blob の名前 (Sum) ごとの順番待ち. 同じ名前には同時に1つしか入れないが, 違う名前どうしは互いに待たない.
type sumLocks struct {
	mu      sync.Mutex
	entries map[Sum]*sumLockEntry
}

type sumLockEntry struct {
	token chan struct{} // 容量1. 値が入っている間は誰かが使っている
	refs  int           // 使っている1つと, 待っている数の合計. 0 になったら entries から消す
}

// lock は, sum の番が来るまで待ち, 番を返す関数を返す. 待っている間に ctx が終わったら, 並ぶのをやめて ctx のエラーを返す.
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
