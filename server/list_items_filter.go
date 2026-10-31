package server

import (
	"errors"
	"fmt"
	"time"

	"go.einride.tech/aip/filtering"
	expr "google.golang.org/genproto/googleapis/api/expr/v1alpha1"

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
func parseItemFilter(s string) (itemstore.ListOptions, error) {
	var o itemstore.ListOptions
	f, err := filtering.ParseFilterString(s, listItemsFilterDeclarations)
	if err != nil {
		return o, err
	}
	if e := f.CheckedExpr.GetExpr(); e != nil {
		if err := addFilterCondition(&o, e); err != nil {
			return o, err
		}
	}
	return o, nil
}

func addFilterCondition(o *itemstore.ListOptions, e *expr.Expr) error {
	call := e.GetCallExpr()
	if call == nil {
		return errors.New("条件には比較か, 比較を AND で結合したもののみを指定できます")
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
	case filtering.FunctionEquals:
		return addKindCondition(o, args)
	case filtering.FunctionGreaterEquals, filtering.FunctionGreaterThan, filtering.FunctionLessThan, filtering.FunctionLessEquals:
		return addUpdateTimeCondition(o, fn, args)
	default:
		return fmt.Errorf("%s は使用できません (使用できるのは AND, =, <, <=, >, >= です)", fn)
	}
}

func addKindCondition(o *itemstore.ListOptions, args []*expr.Expr) error {
	if len(args) != 2 || qualifiedName(args[0]) != filterFieldKind {
		return fmt.Errorf("= は, 左辺が %s の場合のみ使用できます", filterFieldKind)
	}
	if o.Kind != "" {
		return fmt.Errorf("%s は1回のみ指定できます", filterFieldKind)
	}
	name := args[1].GetIdentExpr().GetName()
	switch apiv1alpha1.ItemKind(apiv1alpha1.ItemKind_value[name]) {
	case apiv1alpha1.ItemKind_ITEM_KIND_ASSET:
		o.Kind = itemstore.KindAsset
	case apiv1alpha1.ItemKind_ITEM_KIND_COLLECTION:
		o.Kind = itemstore.KindCollection
	default:
		return fmt.Errorf("%s には ITEM_KIND_ASSET か ITEM_KIND_COLLECTION を指定してください", filterFieldKind)
	}
	return nil
}

// addUpdateTimeCondition は, 時刻の比較を, itemstore の "以降 (含む)" と "より前 (含まない)" の2つに変換する. 上限または下限を複数回指定した場合は, 範囲が狭くなる方を採用する.
//
// 時刻はナノ秒単位の整数なので, "> t" は ">= t+1ns" と, "<= t" は "< t+1ns" と等価になる. ミリ秒への切り上げは itemstore が行う.
func addUpdateTimeCondition(o *itemstore.ListOptions, fn string, args []*expr.Expr) error {
	if len(args) != 2 || qualifiedName(args[0]) != filterFieldUpdateTime {
		return fmt.Errorf("%s は, 左辺が %s の場合のみ使用できます", fn, filterFieldUpdateTime)
	}
	t, err := timestampLiteral(args[1])
	if err != nil {
		return err
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

// timestampLiteral は, "2026-10-01T00:00:00Z" のような文字列か, timestamp("...") の呼び出しから時刻を読み取る. 形式が RFC 3339 であることは, 型検査で確認済み.
func timestampLiteral(e *expr.Expr) (time.Time, error) {
	if call := e.GetCallExpr(); call != nil && call.GetFunction() == filtering.FunctionTimestamp && len(call.GetArgs()) == 1 {
		e = call.GetArgs()[0]
	}
	c := e.GetConstExpr()
	if c == nil {
		return time.Time{}, errors.New(`時刻は "2026-10-01T00:00:00Z" のような RFC 3339 の文字列で指定してください`)
	}
	return time.Parse(time.RFC3339Nano, c.GetStringValue())
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
