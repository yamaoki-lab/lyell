package itemstore

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	storagev1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/storage/v1alpha1"
)

// encodeBody は, 記録する body を作る. c.Body を RevisionBody として読み, revisions の中身の列に写すフィールドを c の値で上書きしてバイト列に戻す. c.Body にある他のフィールドは, この Lyell が知らないものも含めてそのまま残す.
func encodeBody(c Content) ([]byte, error) {
	var b storagev1alpha1.RevisionBody
	if err := proto.Unmarshal(c.Body, &b); err != nil {
		return nil, fmt.Errorf("%w: Body を RevisionBody として読めません: %v", ErrInvalid, err)
	}
	b.CreateTime = timestamppb.New(time.UnixMilli(c.CreatedAt.UnixMilli()))
	b.AuthorUser = c.AuthorUser
	b.AuthorMachine = c.AuthorMachine
	b.BlobSha256 = nil
	b.BlobSize = 0
	if c.hasBlob() {
		b.BlobSha256 = append([]byte(nil), c.Blob[:]...)
		b.BlobSize = c.BlobSize
	}
	// 同じ中身から同じバイト列ができるよう, フィールドの順序を固定する.
	return proto.MarshalOptions{Deterministic: true}.Marshal(&b)
}

// bodyColumns は, body から求めた revisions の中身の列の値.
type bodyColumns struct {
	createdAt     int64 // Unix ミリ秒 (UTC)
	authorUser    string
	authorMachine string
	blob          []byte // blob を持たない revision では nil
	blobSize      int64
}

// blobSizeColumn は, blob_size の列に書く値を返す. blob_sha256 と同じく, blob を持たない revision では NULL にする.
func (c bodyColumns) blobSizeColumn() any {
	if c.blob == nil {
		return nil
	}
	return c.blobSize
}

// columnsFromBody は, body から revisions の中身の列の値を求める. 書き込み, 検証, および列を追加するスキーマ版でのデータ移行は, どれもこの関数を使う. 列と body が食い違わないようにするため.
func columnsFromBody(body []byte) (bodyColumns, error) {
	var b storagev1alpha1.RevisionBody
	if err := proto.Unmarshal(body, &b); err != nil {
		return bodyColumns{}, fmt.Errorf("body を RevisionBody として読めません: %w", err)
	}
	// blob を持たない body では, 長さ0のフィールドを明示的に持つ場合も含めて GetBlobSha256 は nil を返すので, 列には長さ0の BLOB ではなく NULL が入る.
	return bodyColumns{
		createdAt:     b.GetCreateTime().AsTime().UnixMilli(),
		authorUser:    b.GetAuthorUser(),
		authorMachine: b.GetAuthorMachine(),
		blob:          b.GetBlobSha256(),
		blobSize:      b.GetBlobSize(),
	}, nil
}
