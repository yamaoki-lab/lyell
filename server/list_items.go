package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"

	"connectrpc.com/connect"
	"go.einride.tech/aip/ordering"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/internal/itemstore"
)

// ListItems の page_size の既定値と上限値. 値は lyell#21 に基づく.
const (
	defaultPageSize = 128
	maxPageSize     = 1024
)

func (s *itemService) ListItems(ctx context.Context, req *connect.Request[apiv1alpha1.ListItemsRequest]) (*connect.Response[apiv1alpha1.ListItemsResponse], error) {
	msg := req.Msg
	limit, err := pageSize(msg.GetPageSize())
	if err != nil {
		return nil, invalidArgument("page_size", err)
	}
	order, err := parseItemOrder(msg.GetOrderBy())
	if err != nil {
		return nil, invalidArgument("order_by", err)
	}
	opts, err := parseItemFilter(msg.GetFilter())
	if err != nil {
		return nil, invalidArgument("filter", err)
	}
	opts.Order, opts.Limit = order, limit

	sum := requestChecksum(msg)
	if t := msg.GetPageToken(); t != "" {
		if opts.StartAfter, err = decodePageToken(t, sum); err != nil {
			return nil, invalidArgument("page_token", err)
		}
	}

	list, err := s.items.ListItems(ctx, opts)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	resp := &apiv1alpha1.ListItemsResponse{TotalSize: int32(min(list.TotalSize, math.MaxInt32))}
	for _, e := range list.Entries {
		head := revisionToProto(e.Head)
		// extension_data は1件あたり最大 16 KiB なので, 1024 件のページでは 16 MiB に達する. 一覧では返さない.
		if head.Content != nil {
			head.Content.ExtensionData = nil
		}
		resp.Entries = append(resp.Entries, &apiv1alpha1.ItemEntry{Item: itemToProto(e.Item), HeadRevision: head})
	}
	if list.More {
		resp.NextPageToken = encodePageToken(list.Entries[len(list.Entries)-1].Item.ID, sum)
	}
	return connect.NewResponse(resp), nil
}

// pageSize は, page_size を itemstore に渡す件数に変換する. 0 は既定値とし, 上限を超える値は上限値に切り詰める (AIP-158).
func pageSize(n int32) (int, error) {
	switch {
	case n < 0:
		return 0, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_PAGE_SIZE_NEGATIVE, fmt.Errorf("負の値 %d は指定できません", n))
	case n == 0:
		return defaultPageSize, nil
	}
	return min(int(n), maxPageSize), nil
}

// orderFieldID は, ListItems の order_by で使えるフィールド. フィールドパスは ItemEntry を起点とする.
const orderFieldID = "item.id"

// parseItemOrder は, order_by (AIP-132) を itemstore の並び順に変換する. 現在は item.id のみ指定できる. 空の場合は新しい順とする.
func parseItemOrder(s string) (itemstore.Order, error) {
	// ordering は "asc" も受け付けるが, AIP-132 で規定されているのは "desc" のみであるため拒否する. 同一の並び順を表す指定方法を一意に保つため.
	for _, field := range strings.Split(s, ",") {
		if parts := strings.Fields(field); len(parts) == 2 && parts[1] == "asc" {
			return 0, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_ORDER_BY_SYNTAX, fmt.Errorf(`"asc" は指定できません. 古い順は %q と指定してください`, orderFieldID))
		}
	}
	var ob ordering.OrderBy
	if err := ob.UnmarshalString(s); err != nil {
		return 0, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_ORDER_BY_SYNTAX, err)
	}
	for _, f := range ob.Fields {
		switch {
		case f.Path == orderFieldID:
		case !itemEntryHasField(f.Path):
			return 0, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_ORDER_BY_FIELD_UNKNOWN, fmt.Errorf("%s は ItemEntry にないフィールドです", f.Path))
		default:
			return 0, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_ORDER_BY_FIELD_UNSUPPORTED, fmt.Errorf("%s はまだ並び順に使用できません (使用できるのは %s のみです)", f.Path, orderFieldID))
		}
	}
	switch len(ob.Fields) {
	case 0:
		return itemstore.OrderIDDescending, nil
	case 1:
		if ob.Fields[0].Desc {
			return itemstore.OrderIDDescending, nil
		}
		return itemstore.OrderIDAscending, nil
	}
	return 0, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_ORDER_BY_TOO_MANY_FIELDS, errors.New("並び順に指定できるフィールドは1つのみです"))
}

// page token は, 続きの位置 (前ページの末尾の item の ID) と, リクエスト条件の checksum を base64 エンコードしたもの. クライアントには中身を公開しない (AIP-158).
//
// checksum は, 条件を変えて page token が再利用された場合に拒否するためのもの. AIP-158 では, page_size 以外のフィールドが異なる場合は INVALID_ARGUMENT を返すよう規定されている.
// 署名はしないため, クライアントは任意の ID から読み出しを開始する page token を作成できる. 取得できるのはその位置から読み取れる item のみであるため, 権限の問題は発生しない (認証を実装する際に見直す).
const pageTokenVersion = 1

type pageTokenChecksum [8]byte

// requestChecksum は, page token の作成時と使用時で一致しているべきフィールド (filter と order_by) の SHA-256 の先頭8バイトを返す.
func requestChecksum(msg *apiv1alpha1.ListItemsRequest) pageTokenChecksum {
	h := sha256.New()
	// 区切りとして 0 バイトを挟み, filter と order_by の境界が異なる組み合わせで同一の値にならないようにする.
	h.Write([]byte(msg.GetFilter()))
	h.Write([]byte{0})
	h.Write([]byte(msg.GetOrderBy()))
	var sum pageTokenChecksum
	copy(sum[:], h.Sum(nil))
	return sum
}

func encodePageToken(after itemstore.ID, sum pageTokenChecksum) string {
	b := make([]byte, 0, 1+len(after)+len(sum))
	b = append(b, pageTokenVersion)
	b = append(b, after[:]...)
	b = append(b, sum[:]...)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePageToken(s string, want pageTokenChecksum) (itemstore.ID, error) {
	var (
		after itemstore.ID
		sum   pageTokenChecksum
	)
	// Strict: 未使用ビットが 0 でない表現を拒否し, 同じ page token の表現を一意に保つ.
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != 1+len(after)+len(sum) || b[0] != pageTokenVersion {
		return itemstore.ID{}, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_PAGE_TOKEN_MALFORMED, errors.New("page token の形式が不正です"))
	}
	copy(after[:], b[1:])
	copy(sum[:], b[1+len(after):])
	if sum != want {
		return itemstore.ID{}, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_PAGE_TOKEN_MISMATCH, errors.New("page token の作成時と filter または order_by が異なります"))
	}
	return after, nil
}
