package server

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	storagev1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/storage/v1alpha1"
)

// nameField は, RevisionContent の name のフィールド名. updatedFields が返す集合を調べる時に使う.
const nameField = "name"

// bodyName は, revision の body に保存された名前を返す. itemstore は読めない body を記録しないので, 読めない時は名前が無いものとして扱う.
func bodyName(body []byte) string {
	var b storagev1alpha1.RevisionBody
	if err := proto.Unmarshal(body, &b); err != nil {
		return ""
	}
	return b.GetName()
}

// extensionDataField は, RevisionContent の extension_data のフィールド名. updatedFields が返す集合を調べる時に使う.
const extensionDataField = "extension_data"

// bodyExtensionData は, revision の body に保存された, 拡張機能のみが読み取るデータを返す. 読めない時は, bodyName と同じく無いものとして扱う.
func bodyExtensionData(body []byte) []byte {
	var b storagev1alpha1.RevisionBody
	if err := proto.Unmarshal(body, &b); err != nil {
		return nil
	}
	return b.GetExtensionData()
}

// withName は, body の名前を name に置き換えた body を返す. 他のフィールドは, この Lyell が知らないものも含めて残す. body が nil なら, 名前だけを持つ body を作る.
func withName(body []byte, name string) ([]byte, error) {
	var b storagev1alpha1.RevisionBody
	if err := proto.Unmarshal(body, &b); err != nil {
		return nil, fmt.Errorf("前の revision の body を読めません: %w", err)
	}
	b.Name = name
	return proto.MarshalOptions{Deterministic: true}.Marshal(&b)
}
