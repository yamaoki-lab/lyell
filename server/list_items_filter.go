package server

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.einride.tech/aip/filtering"
	expr "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
	"google.golang.org/protobuf/reflect/protoreflect"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/internal/itemstore"
)

// ListItems の filter で使えるフィールド. フィールドパスは ItemEntry を起点とする.
const (
	filterFieldKind       = "item.kind"
	filterFieldUpdateTime = "head_revision.content.create_time"
)

// listItemsFilterDeclarations は, filter の型検査に使う宣言. item.kind は ItemKind の値の名前 (ITEM_KIND_ASSET 等) と比較し, head_revision.content.create_time は RFC 3339 の時刻と比較する.
var listItemsFilterDeclarations = func() *filtering.Declarations {
	d, err := filtering.NewDeclarations(
		filtering.DeclareStandardFunctions(),
		filtering.DeclareEnumIdent(filterFieldKind, apiv1alpha1.ItemKind(0).Type()),
		filtering.DeclareIdent(filterFieldUpdateTime, filtering.TypeTimestamp),
	)
	if err != nil {
		panic(err) // 宣言は固定なので, 失敗する場合はこのファイルの実装の誤り
	}
	return d
}()

// parseItemFilter は, filter (AIP-160) を itemstore の条件に変換する. 現在サポートしているのは, item.kind の等値比較と, head_revision.content.create_time の比較を AND で結合したもののみである.
//
// 型検査の前に, 構文解析した式を 形 → フィールド → 演算子 → 値 の順にたどり, 拒否の理由を決める. 型検査は, 未知のフィールドと未サポートのフィールドをどちらも未宣言の識別子として, 型の合わない値とそのフィールドに使えない演算子をどちらもオーバーロードの欠落として報告し, 理由を区別できないため.
// たどった後の型検査は念のためのもので, それでも失敗する式は値の型によるものとして FILTER_VALUE_INVALID で拒否する.
func parseItemFilter(s string) (itemstore.ListOptions, error) {
	var o itemstore.ListOptions
	if s == "" {
		return o, nil
	}
	var p filtering.Parser
	p.Init(s)
	parsed, err := p.Parse()
	if err != nil {
		return o, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_SYNTAX, err)
	}
	if err := addFilterCondition(&o, parsed.GetExpr()); err != nil {
		return o, err
	}
	var c filtering.Checker
	c.Init(parsed.GetExpr(), parsed.GetSourceInfo(), listItemsFilterDeclarations)
	if _, err := c.Check(); err != nil {
		return o, reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_VALUE_INVALID, err)
	}
	return o, nil
}

func addFilterCondition(o *itemstore.ListOptions, e *expr.Expr) error {
	call := e.GetCallExpr()
	if call == nil {
		return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_OPERATOR_UNSUPPORTED, errors.New("条件には比較か, 比較を AND で結合したもののみを指定できます"))
	}
	args := call.GetArgs()
	switch fn := call.GetFunction(); fn {
	case filtering.FunctionAnd:
		for _, a := range args {
			if err := addFilterCondition(o, a); err != nil {
				return err
			}
		}
		return nil
	case filtering.FunctionEquals, filtering.FunctionGreaterEquals, filtering.FunctionGreaterThan, filtering.FunctionLessThan, filtering.FunctionLessEquals:
		return addComparison(o, fn, args)
	default:
		return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_OPERATOR_UNSUPPORTED, fmt.Errorf("%s は使用できません (使用できるのは AND, =, <, <=, >, >= です)", fn))
	}
}

// addComparison は, 比較1つを, 左辺のフィールド → 演算子 → 右辺の値 の順に確かめて条件に加える.
func addComparison(o *itemstore.ListOptions, fn string, args []*expr.Expr) error {
	field := ""
	if len(args) == 2 {
		field = qualifiedName(args[0])
	}
	if field == "" {
		return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_OPERATOR_UNSUPPORTED, fmt.Errorf("%s は, 左辺がフィールドの場合のみ使用できます", fn))
	}
	switch field {
	case filterFieldKind:
		if fn != filtering.FunctionEquals {
			return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_OPERATOR_UNSUPPORTED, fmt.Errorf("%s には = のみ使用できます", filterFieldKind))
		}
		return addKindCondition(o, args[1])
	case filterFieldUpdateTime:
		if fn == filtering.FunctionEquals {
			return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_OPERATOR_UNSUPPORTED, fmt.Errorf("%s には <, <=, >, >= のみ使用できます", filterFieldUpdateTime))
		}
		return addUpdateTimeCondition(o, fn, args[1])
	}
	if !itemEntryHasField(field) {
		return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_FIELD_UNKNOWN, fmt.Errorf("%s は ItemEntry にないフィールドです", field))
	}
	return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_FIELD_UNSUPPORTED, fmt.Errorf("%s はまだ絞り込みに使用できません (使用できるのは %s と %s です)", field, filterFieldKind, filterFieldUpdateTime))
}

func addKindCondition(o *itemstore.ListOptions, value *expr.Expr) error {
	name := value.GetIdentExpr().GetName()
	var kind itemstore.Kind
	switch apiv1alpha1.ItemKind(apiv1alpha1.ItemKind_value[name]) {
	case apiv1alpha1.ItemKind_ITEM_KIND_ASSET:
		kind = itemstore.KindAsset
	case apiv1alpha1.ItemKind_ITEM_KIND_COLLECTION:
		kind = itemstore.KindCollection
	default:
		return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_VALUE_INVALID, fmt.Errorf("%s には ITEM_KIND_ASSET か ITEM_KIND_COLLECTION を指定してください", filterFieldKind))
	}
	if o.Kind != "" {
		return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_FIELD_REPEATED, fmt.Errorf("%s は1回のみ指定できます", filterFieldKind))
	}
	o.Kind = kind
	return nil
}

// addUpdateTimeCondition は, 時刻の比較を, itemstore の "以降 (含む)" と "より前 (含まない)" の2つに変換する. 上限または下限を複数回指定した場合は, 範囲が狭くなる方を採用する.
//
// 時刻はナノ秒単位の整数なので, "> t" は ">= t+1ns" と, "<= t" は "< t+1ns" と等価になる. ミリ秒への切り上げは itemstore が行う.
func addUpdateTimeCondition(o *itemstore.ListOptions, fn string, value *expr.Expr) error {
	t, err := timestampLiteral(value)
	if err != nil {
		return reject(apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_FILTER_VALUE_INVALID, err)
	}
	switch fn {
	case filtering.FunctionGreaterThan:
		t = t.Add(time.Nanosecond)
		fallthrough
	case filtering.FunctionGreaterEquals:
		if o.UpdatedSince.IsZero() || t.After(o.UpdatedSince) {
			o.UpdatedSince = t
		}
	case filtering.FunctionLessEquals:
		t = t.Add(time.Nanosecond)
		fallthrough
	case filtering.FunctionLessThan:
		if o.UpdatedBefore.IsZero() || t.Before(o.UpdatedBefore) {
			o.UpdatedBefore = t
		}
	}
	return nil
}

// timestampLiteral は, "2026-10-01T00:00:00Z" のような RFC 3339 の文字列か, timestamp("...") の呼び出しから時刻を読み取る.
func timestampLiteral(e *expr.Expr) (time.Time, error) {
	if call := e.GetCallExpr(); call != nil && call.GetFunction() == filtering.FunctionTimestamp && len(call.GetArgs()) == 1 {
		e = call.GetArgs()[0]
	}
	c, ok := e.GetConstExpr().GetConstantKind().(*expr.Constant_StringValue)
	if !ok {
		return time.Time{}, errors.New(`時刻は "2026-10-01T00:00:00Z" のような RFC 3339 の文字列で指定してください`)
	}
	t, err := time.Parse(time.RFC3339, c.StringValue)
	if err != nil {
		return time.Time{}, fmt.Errorf(`時刻は "2026-10-01T00:00:00Z" のような RFC 3339 の文字列で指定してください: %w`, err)
	}
	return t, nil
}

// qualifiedName は, item.kind のようなフィールド選択式を, ドットで連結した名前に変換する. 識別子でなければ空文字列を返す.
func qualifiedName(e *expr.Expr) string {
	switch k := e.GetExprKind().(type) {
	case *expr.Expr_IdentExpr:
		return k.IdentExpr.GetName()
	case *expr.Expr_SelectExpr:
		if parent := qualifiedName(k.SelectExpr.GetOperand()); parent != "" {
			return parent + "." + k.SelectExpr.GetField()
		}
	}
	return ""
}

// itemEntryHasField は, ItemEntry を起点とするフィールドパス (item.kind など) が, ItemEntry の記述子に存在するかを返す. filter と order_by で, 未知のフィールドと, 存在するがまだ使えないフィールドを区別するのに使う. 記述子で引くので, 後から ItemEntry に足したフィールドは, ここを変えなくても未サポートとして扱われる.
func itemEntryHasField(path string) bool {
	md := (&apiv1alpha1.ItemEntry{}).ProtoReflect().Descriptor()
	segments := strings.Split(path, ".")
	for i, seg := range segments {
		fd := md.Fields().ByName(protoreflect.Name(seg))
		if fd == nil {
			return false
		}
		if i == len(segments)-1 {
			return true
		}
		if md = fd.Message(); md == nil {
			return false
		}
	}
	return false
}
