package server

import (
	"errors"
	"fmt"
	"slices"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
)

// blobSha256Field は, RevisionContent の blob_sha256 のフィールド名. updatedFields が返す集合を調べる時に使う.
const blobSha256Field = "blob_sha256"

// updatedFields は, CommitRevision で変更する content のフィールド名の集合を, update_mask (AIP-134) から決める. mask が省略されたか空なら, content で値が設定されているフィールドをその集合に入れる.
// 出力専用のフィールドも集合に入りうるが, server はその値を読まずに自分で決めるので, 結果として無視される (AIP-203).
func updatedFields(mask *fieldmaskpb.FieldMask, content *apiv1alpha1.RevisionContent) (map[string]bool, error) {
	// content が nil でも, ProtoReflect は空で読み取り専用のメッセージを返す.
	m := content.ProtoReflect()
	updated := map[string]bool{}
	if len(mask.GetPaths()) == 0 {
		m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
			updated[string(fd.Name())] = true
			return true
		})
		return updated, nil
	}
	fields := m.Descriptor().Fields()
	for i, p := range mask.GetPaths() {
		field := fmt.Sprintf("update_mask.paths[%d]", i)
		if p == "*" {
			// 全置換を許すと, クライアントが知らないフィールドまで消去してしまう.
			return nil, invalidArgument(field, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_UPDATE_MASK_WILDCARD, errors.New("ワイルドカード (*) は指定できません. 変更するフィールドを列挙してください")))
		}
		if fields.ByName(protoreflect.Name(p)) == nil {
			return nil, invalidArgument(field, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_UPDATE_MASK_FIELD_UNKNOWN, fmt.Errorf("content にフィールド %q はありません", p)))
		}
		updated[p] = true
	}
	return updated, nil
}

// sameInput は, a と b が, 出力専用でないすべてのフィールドで等しいかを返す. 出力専用のフィールド (記録した時刻等) は revision ごとに server が決めるので, 比べない.
func sameInput(a, b *apiv1alpha1.RevisionContent) bool {
	ma, mb := a.ProtoReflect(), b.ProtoReflect()
	fields := ma.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if !outputOnly(fd) && !ma.Get(fd).Equal(mb.Get(fd)) {
			return false
		}
	}
	return true
}

// outputOnly は, fd に google.api.field_behavior の OUTPUT_ONLY が付いているかを返す.
func outputOnly(fd protoreflect.FieldDescriptor) bool {
	behaviors, _ := proto.GetExtension(fd.Options(), annotations.E_FieldBehavior).([]annotations.FieldBehavior)
	return slices.Contains(behaviors, annotations.FieldBehavior_OUTPUT_ONLY)
}
