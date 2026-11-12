package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/internal/blobstore"
	"github.com/yamaoki-lab/lyell/internal/itemstore"
)

// itemService は, ItemService の実装. itemstore を呼び, Protobuf のメッセージとの変換とエラーコードの対応だけを担う.
type itemService struct {
	items *itemstore.Store
	log   *slog.Logger
}

func (s *itemService) CreateItem(ctx context.Context, req *connect.Request[apiv1alpha1.CreateItemRequest]) (*connect.Response[apiv1alpha1.CreateItemResponse], error) {
	kind, err := parseKind(req.Msg.GetKind())
	if err != nil {
		return nil, invalidArgument("kind", err)
	}
	sum, err := parseSum(req.Msg.GetContent().GetBlobSha256())
	if err != nil {
		return nil, invalidArgument("content.blob_sha256", err)
	}
	item, err := s.items.CreateItem(ctx, itemstore.NewItem{Kind: kind, Content: newContent(ctx, sum)})
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	rev, err := s.items.Revision(ctx, item.Head)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	return connect.NewResponse(&apiv1alpha1.CreateItemResponse{Item: itemToProto(item), Revision: revisionToProto(rev)}), nil
}

func (s *itemService) GetItem(ctx context.Context, req *connect.Request[apiv1alpha1.GetItemRequest]) (*connect.Response[apiv1alpha1.GetItemResponse], error) {
	id, err := parseID(req.Msg.GetId())
	if err != nil {
		return nil, invalidArgument("id", err)
	}
	item, err := s.items.Item(ctx, id)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	return connect.NewResponse(&apiv1alpha1.GetItemResponse{Item: itemToProto(item)}), nil
}

func (s *itemService) GetRevision(ctx context.Context, req *connect.Request[apiv1alpha1.GetRevisionRequest]) (*connect.Response[apiv1alpha1.GetRevisionResponse], error) {
	id, err := parseID(req.Msg.GetId())
	if err != nil {
		return nil, invalidArgument("id", err)
	}
	rev, err := s.items.Revision(ctx, id)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	return connect.NewResponse(&apiv1alpha1.GetRevisionResponse{Revision: revisionToProto(rev)}), nil
}

func (s *itemService) CommitRevision(ctx context.Context, req *connect.Request[apiv1alpha1.CommitRevisionRequest]) (*connect.Response[apiv1alpha1.CommitRevisionResponse], error) {
	itemID, err := parseID(req.Msg.GetItemId())
	if err != nil {
		return nil, invalidArgument("item_id", err)
	}
	from, err := parseID(req.Msg.GetExpectedHead())
	if err != nil {
		return nil, invalidArgument("expected_head", err)
	}
	var parents []itemstore.ID
	for i, p := range req.Msg.GetParents() {
		id, err := parseID(p)
		if err != nil {
			return nil, invalidArgument(fmt.Sprintf("parents[%d]", i), err)
		}
		parents = append(parents, id)
	}
	sum, err := parseSum(req.Msg.GetContent().GetBlobSha256())
	if err != nil {
		return nil, invalidArgument("content.blob_sha256", err)
	}
	revID, err := s.items.CommitRevision(ctx, itemstore.NewRevision{ItemID: itemID, From: from, Parents: parents, Content: newContent(ctx, sum)})
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	rev, err := s.items.Revision(ctx, revID)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	return connect.NewResponse(&apiv1alpha1.CommitRevisionResponse{Revision: revisionToProto(rev)}), nil
}

// newContent は, API から記録する revision の中身を作成する. 時刻と author はサーバが決め, 呼び出し側には指定させない. revision の body は, 形式 (lyell.storage.v1alpha1) を決めるまで空にする.
func newContent(ctx context.Context, sum blobstore.Sum) itemstore.Content {
	user, machine := author(ctx)
	return itemstore.Content{CreatedAt: time.Now(), AuthorUser: user, AuthorMachine: machine, Blob: sum}
}

// connectError は, itemstore のエラーを Connect のエラーコードに対応させる. 呼び出し側の誤りはメッセージをそのまま返す. それ以外は内部エラーとして記録し, 詳細 (DB のパス等) を外に出さない.
func (s *itemService) connectError(ctx context.Context, err error) error {
	var (
		nff *itemstore.NotFastForwardError
		pe  *itemstore.ParentsError
	)
	switch {
	case errors.As(err, &pe):
		return parentsError(pe)
	case errors.As(err, &nff):
		ce := connect.NewError(connect.CodeAborted, err)
		// 呼び出し側が読み直さずに現在の head を取得できるよう, 詳細に HeadMoved を付ける.
		if d, derr := connect.NewErrorDetail(&apiv1alpha1.HeadMoved{CurrentHead: nff.Head.String()}); derr == nil {
			ce.AddDetail(d)
		}
		return ce
	case errors.Is(err, itemstore.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, itemstore.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, itemstore.ErrBlobNotFound):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	s.log.ErrorContext(ctx, "API の処理中の内部エラー", "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("内部エラー"))
}

// rejection は, 呼び出し側の誤りに, 拒否の理由 (BadRequest の reason) を添えたエラー.
type rejection struct {
	reason apiv1alpha1.FieldViolationReason
	err    error
}

func (r *rejection) Error() string { return r.err.Error() }
func (r *rejection) Unwrap() error { return r.err }

func reject(reason apiv1alpha1.FieldViolationReason, err error) error {
	return &rejection{reason: reason, err: err}
}

// reasonPrefix は, FieldViolationReason の値の名前の接頭辞. 同じパッケージの他の enum と値の名前が衝突しないよう, buf の lint (ENUM_VALUE_PREFIX) でこの接頭辞が求められる.
const reasonPrefix = "FIELD_VIOLATION_REASON_"

// reasonName は, BadRequest の reason に設定する文字列を返す. 値の名前から接頭辞を除いたもの.
func reasonName(r apiv1alpha1.FieldViolationReason) string {
	return strings.TrimPrefix(r.String(), reasonPrefix)
}

// invalidArgument は, INVALID_ARGUMENT のエラーを作り, 詳細情報に BadRequest (AIP-193) を付ける. field はリクエスト内のフィールドのパス. err が reject で作ったエラーなら, その理由を reason に設定する.
func invalidArgument(field string, err error) error {
	ce := connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", field, err))
	v := &errdetails.BadRequest_FieldViolation{Field: field, Description: err.Error()}
	var r *rejection
	if errors.As(err, &r) {
		v.Reason = reasonName(r.reason)
	}
	if d, derr := connect.NewErrorDetail(&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{v}}); derr == nil {
		ce.AddDetail(d)
	}
	return ce
}

// parentsError は, itemstore が親の指定を拒否したエラーを, どのフィールドがどの決まりに反したかと共に返す.
func parentsError(pe *itemstore.ParentsError) error {
	switch pe.Problem {
	case itemstore.ParentsTooMany:
		return invalidArgument("parents", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_PARENTS_TOO_MANY, pe))
	case itemstore.ParentsDuplicate:
		return invalidArgument(fmt.Sprintf("parents[%d]", pe.Index), reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_PARENTS_DUPLICATE, pe))
	case itemstore.ParentsMissingFrom:
		return invalidArgument("parents", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_PARENTS_MISSING_EXPECTED_HEAD, pe))
	}
	return invalidArgument("parents", pe)
}

// parseID は, ハイフン区切りの36文字 (小文字) の ID を読み取る. uuid.Parse は波括弧や urn:uuid: 付きの表記, 大文字の16進数も受け付けるが, API の表記を統一するため, 応答で返す表記 (ID.String) と一致しないものは拒否する.
func parseID(s string) (itemstore.ID, error) {
	const reason = apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_ID_MALFORMED
	if len(s) != 36 {
		return itemstore.ID{}, reject(reason, fmt.Errorf("ID はハイフン区切りの36文字ですが, %d文字でした", len(s)))
	}
	u, err := uuid.Parse(s)
	if err != nil {
		return itemstore.ID{}, reject(reason, err)
	}
	if u.String() != s {
		return itemstore.ID{}, reject(reason, errors.New("ID は小文字で表記してください"))
	}
	return itemstore.ID(u), nil
}

// parseSum は, 小文字の16進数64文字の SHA-256 を読み取る. blobstore.ParseSum は大文字も受け付けるが, blob の URL がキャッシュのキーになるので, 同じ blob に対して複数の表記を許可しない. 入力が空なら, 必須フィールドの省略として拒否する.
func parseSum(s string) (blobstore.Sum, error) {
	if s == "" {
		return blobstore.Sum{}, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_BLOB_SHA256_REQUIRED, errors.New("中身の SHA-256 が指定されていません"))
	}
	const reason = apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_BLOB_SHA256_MALFORMED
	sum, err := blobstore.ParseSum(s)
	if err != nil {
		return blobstore.Sum{}, reject(reason, err)
	}
	if sum.String() != s {
		return blobstore.Sum{}, reject(reason, errors.New("SHA-256 は小文字の16進数で表記してください"))
	}
	return sum, nil
}

// parseKind は, API の item の種類を itemstore の種類に変換する. collection は一覧の形式が決まるまで作成できない.
func parseKind(k apiv1alpha1.ItemKind) (itemstore.Kind, error) {
	switch k {
	case apiv1alpha1.ItemKind_ITEM_KIND_ASSET:
		return itemstore.KindAsset, nil
	case apiv1alpha1.ItemKind_ITEM_KIND_COLLECTION:
		return "", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_KIND_UNSUPPORTED, errors.New("collection は, 一覧の形式が決まるまで作成できません"))
	case apiv1alpha1.ItemKind_ITEM_KIND_UNSPECIFIED:
		return "", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_KIND_UNSPECIFIED, errors.New("item の種類が指定されていません"))
	}
	return "", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_KIND_UNKNOWN, fmt.Errorf("item の種類 %v は受け付けられません", k))
}

func itemToProto(item itemstore.Item) *apiv1alpha1.Item {
	kind := apiv1alpha1.ItemKind_ITEM_KIND_UNSPECIFIED
	switch item.Kind {
	case itemstore.KindAsset:
		kind = apiv1alpha1.ItemKind_ITEM_KIND_ASSET
	case itemstore.KindCollection:
		kind = apiv1alpha1.ItemKind_ITEM_KIND_COLLECTION
	}
	return &apiv1alpha1.Item{Id: item.ID.String(), Kind: kind, Head: item.Head.String()}
}

func revisionToProto(rev itemstore.Revision) *apiv1alpha1.Revision {
	parents := make([]string, len(rev.Parents))
	for i, p := range rev.Parents {
		parents[i] = p.String()
	}
	out := &apiv1alpha1.Revision{Id: rev.ID.String(), ItemId: rev.ItemID.String(), Parents: parents}
	// 間引かれた revision は content を持たない.
	if c := rev.Content; c != nil {
		out.Content = &apiv1alpha1.RevisionContent{
			CreateTime:    timestamppb.New(c.CreatedAt),
			AuthorUser:    c.AuthorUser,
			AuthorMachine: c.AuthorMachine,
			BlobSha256:    c.Blob.String(),
			BlobSize:      c.BlobSize,
		}
	}
	return out
}
