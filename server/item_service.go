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
	// 作成できるのは asset のみなので, 拡張機能用のフィールドは指定されていれば拒否する.
	if req.Msg.GetExtension() != "" {
		return nil, invalidArgument("extension", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_EXTENSION_FOR_ASSET, errors.New("asset には extension を指定できません")))
	}
	if len(req.Msg.GetContent().GetExtensionData()) > 0 {
		return nil, extensionDataForAsset()
	}
	sha := req.Msg.GetContent().GetBlobSha256()
	hasBlob := sha != ""
	var sum blobstore.Sum
	if hasBlob || !req.Msg.GetValidateOnly() {
		sum, err = parseSum(sha)
		if err != nil {
			return nil, invalidArgument("content.blob_sha256", err)
		}
	}
	name, err := checkName(req.Msg.GetContent().GetName())
	if err != nil {
		return nil, err
	}
	body, err := withName(nil, name)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	content := newContent(ctx, sum, body)
	if req.Msg.GetValidateOnly() {
		content, err = s.items.ValidateCreateItem(ctx, kind, "", content, hasBlob)
		if err != nil {
			return nil, s.connectError(ctx, err)
		}
		item := itemToProto(itemstore.Item{Kind: kind})
		item.Id, item.Head = "", ""
		preview := contentToProto(&content)
		preview.CreateTime = nil
		return connect.NewResponse(&apiv1alpha1.CreateItemResponse{
			Item: item, Revision: &apiv1alpha1.Revision{Content: preview},
		}), nil
	}
	item, err := s.items.CreateItem(ctx, itemstore.NewItem{Kind: kind, Content: content})
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
	updated, err := updatedFields(req.Msg.GetUpdateMask(), req.Msg.GetContent())
	if err != nil {
		return nil, err
	}
	// API からコミットできるのは asset のみ. 他の種類の中身の扱い (extension_data の引き継ぎ等) は, まだ決めていない. 接続もトランザクションも持たずに読む.
	item, err := s.items.Item(ctx, itemID)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	if item.Kind != itemstore.KindAsset {
		return nil, invalidArgument("item_id", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_KIND_UNSUPPORTED, fmt.Errorf("%s の item には, まだ revision をコミットできません", item.Kind)))
	}
	if updated[extensionDataField] {
		return nil, extensionDataForAsset()
	}
	var sum blobstore.Sum
	inheritBlob, inheritName := !updated[blobSha256Field], !updated[nameField]
	if !inheritBlob {
		if sum, err = parseSum(req.Msg.GetContent().GetBlobSha256()); err != nil {
			return nil, invalidArgument("content.blob_sha256", err)
		}
	}
	// 名前を変更する時だけ, 新しい名前を自動で修正して検証する. 引き継ぐ名前は再検証しない.
	var newName string
	if !inheritName {
		if newName, err = checkName(req.Msg.GetContent().GetName()); err != nil {
			return nil, err
		}
	}
	// 新しい body は expected_head の body から作り, マージでなければ中身が変わったかどうかも expected_head の中身と比べるため, 引き継ぐフィールドが無い場合も読み取る.
	merge := len(parents) > 1
	head, err := s.expectedHeadContent(ctx, itemID, from, inheritBlob || inheritName)
	if err != nil {
		return nil, err
	}
	// 間引かれていれば, 前の body は残っていないので, 空の body から作る.
	var prev []byte
	if head != nil {
		prev = head.Body
	}
	if inheritBlob {
		sum = head.Blob
	}
	// 名前を変更しなければ, 前の body の名前がそのまま残る. update_mask に name を指定して値を空にすると, 名前を消去する.
	name := bodyName(prev)
	if !inheritName {
		name = newName
	}
	body, err := withName(prev, name)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	// 間引かれていて比べられなければ, 変わったものとして記録する.
	if !merge && head != nil && sameInput(contentToProto(head), &apiv1alpha1.RevisionContent{BlobSha256: sum.String(), Name: name}) {
		return nil, invalidArgument("content", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_REVISION_UNCHANGED, errors.New("expected_head の revision と中身が同じです. 統合でなければ, 変更のない revision は記録できません")))
	}
	input := itemstore.NewRevision{ItemID: itemID, From: from, Parents: parents, Content: newContent(ctx, sum, body)}
	if req.Msg.GetValidateOnly() {
		rev, err := s.items.ValidateCommitRevision(ctx, input)
		if err != nil {
			return nil, s.connectError(ctx, err)
		}
		preview := revisionToProto(rev)
		preview.Id, preview.Content.CreateTime = "", nil
		return connect.NewResponse(&apiv1alpha1.CommitRevisionResponse{Revision: preview}), nil
	}
	revID, err := s.items.CommitRevision(ctx, input)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	rev, err := s.items.Revision(ctx, revID)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	return connect.NewResponse(&apiv1alpha1.CommitRevisionResponse{Revision: revisionToProto(rev)}), nil
}

// expectedHeadContent は, CommitRevision で変更しないフィールドの引き継ぎ元, および変更の有無の比較対象として, expected_head の revision の中身を返す. required が false の場合, 間引かれていれば nil を返す.
// required が true で, 間引かれていて引き継げない時, head が expected_head のままなら FAILED_PRECONDITION を返し, head が動いていれば早送りでない時と同じ ABORTED を返す. 間引かれるのは古い revision であることが多く, 古い expected_head を送った呼び出し側は, 現在の head を知って revision を作り直す必要があるため.
func (s *itemService) expectedHeadContent(ctx context.Context, itemID, from itemstore.ID, required bool) (*itemstore.Content, error) {
	rev, err := s.items.Revision(ctx, from)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	if rev.Content != nil || !required {
		return rev.Content, nil
	}
	item, err := s.items.Item(ctx, itemID)
	if err != nil {
		return nil, s.connectError(ctx, err)
	}
	if item.Head != from {
		return nil, s.connectError(ctx, &itemstore.NotFastForwardError{ItemID: itemID, From: from, Head: item.Head})
	}
	return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("expected_head の revision %s は間引かれているため, 変更しないフィールドを引き継げません", from))
}

// newContent は, API から記録する revision の中身を作成する. 時刻と author はサーバが決め, 呼び出し側には指定させない. 引数 body には, 前の revision の body (最初の revision では nil) を渡す. itemstore が列に写すフィールドを上書きし, この Lyell が知らないフィールドを含む他のフィールドは残すので, 新しい Lyell が書いた revision の上に古い Lyell がコミットしても, 新しいフィールドは失われない.
func newContent(ctx context.Context, sum blobstore.Sum, body []byte) itemstore.Content {
	user, machine := author(ctx)
	return itemstore.Content{CreatedAt: time.Now(), AuthorUser: user, AuthorMachine: machine, Blob: sum, Body: body}
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
		// 呼び出し側が読み直さずに現在の head を取得できるよう, 詳細に HeadMoved を付与する.
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

// extensionDataForAsset は, asset の revision に extension_data が指定された時のエラーを返す.
func extensionDataForAsset() error {
	return invalidArgument("content.extension_data", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_EXTENSION_DATA_FOR_ASSET, errors.New("asset の revision には extension_data を指定できません")))
}

// rejection は, 呼び出し側の誤りに, 拒否理由 (BadRequest の reason) を添えたエラー.
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

// invalidArgument は, INVALID_ARGUMENT のエラーを作り, 詳細情報に BadRequest (AIP-193) を付与する. field はリクエスト内のフィールドのパス. errs はそのフィールドの違反で, 1つの違反を1つの field violation にする. reject で作ったエラーなら, その理由を reason に設定する.
func invalidArgument(field string, errs ...error) *connect.Error {
	msgs := make([]string, len(errs))
	br := &errdetails.BadRequest{}
	for i, err := range errs {
		msgs[i] = err.Error()
		v := &errdetails.BadRequest_FieldViolation{Field: field, Description: err.Error()}
		var r *rejection
		if errors.As(err, &r) {
			v.Reason = reasonName(r.reason)
		}
		br.FieldViolations = append(br.FieldViolations, v)
	}
	// メッセージには, 違反の説明を, 日本語の文の区切りと同じ ". " でつないで並べる.
	ce := connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %s", field, strings.Join(msgs, ". ")))
	if d, derr := connect.NewErrorDetail(br); derr == nil {
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
	case apiv1alpha1.ItemKind_ITEM_KIND_EXTENSION:
		return "", reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_KIND_UNSUPPORTED, errors.New("extension の item は, 拡張機能を登録できるようになるまで作成できません"))
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
	case itemstore.KindExtension:
		kind = apiv1alpha1.ItemKind_ITEM_KIND_EXTENSION
	}
	return &apiv1alpha1.Item{Id: item.ID.String(), Kind: kind, Head: item.Head.String(), Extension: item.Extension}
}

func revisionToProto(rev itemstore.Revision) *apiv1alpha1.Revision {
	parents := make([]string, len(rev.Parents))
	for i, p := range rev.Parents {
		parents[i] = p.String()
	}
	// 間引かれた revision は content を持たない.
	return &apiv1alpha1.Revision{Id: rev.ID.String(), ItemId: rev.ItemID.String(), Parents: parents, Content: contentToProto(rev.Content)}
}

// contentToProto は, revision の中身を API のメッセージにする. 間引かれていて c が nil なら nil を返す.
func contentToProto(c *itemstore.Content) *apiv1alpha1.RevisionContent {
	if c == nil {
		return nil
	}
	// extension の item の revision は blob を持たず, Blob はゼロ値になる. ゼロの SHA-256 を返さず, 空にする.
	var sha string
	if c.Blob != (blobstore.Sum{}) {
		sha = c.Blob.String()
	}
	return &apiv1alpha1.RevisionContent{
		CreateTime:    timestamppb.New(c.CreatedAt),
		AuthorUser:    c.AuthorUser,
		AuthorMachine: c.AuthorMachine,
		BlobSha256:    sha,
		BlobSize:      c.BlobSize,
		Name:          bodyName(c.Body),
		ExtensionData: bodyExtensionData(c.Body),
	}
}
