package server

import (
	"errors"

	"connectrpc.com/connect"

	apiv1alpha1 "github.com/yamaoki-lab/lyell/gen/lyell/api/v1alpha1"
	"github.com/yamaoki-lab/lyell/internal/itemname"
)

// nameReasons は, 名前の規則ごとの拒否理由.
var nameReasons = map[itemname.Rule]apiv1alpha1.FieldViolationReason{
	itemname.RuleForbiddenCharacter: apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_NAME_FORBIDDEN_CHARACTER,
	itemname.RuleBidiControl:        apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_NAME_BIDI_CONTROL,
	itemname.RuleTrailingPeriod:     apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_NAME_TRAILING_PERIOD,
	itemname.RuleReserved:           apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_NAME_RESERVED,
	itemname.RuleTooLong:            apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_NAME_TOO_LONG,
	itemname.RuleBlank:              apiv1alpha1.FieldViolationReason_FIELD_VIOLATION_REASON_NAME_BLANK,
}

// checkName は, リクエストで設定された名前を自動で修正して返す. 修正後の名前が規則を満たさなければ, 満たさない規則をすべて content.name の違反として並べた INVALID_ARGUMENT を返す. 候補を作成できる場合は, 詳細情報に NameSuggestion を付与する.
func checkName(name string) (string, error) {
	fixed, vs := itemname.Check(name)
	if len(vs) == 0 {
		return fixed, nil
	}
	errs := make([]error, len(vs))
	for i, v := range vs {
		errs[i] = reject(nameReasons[v.Rule], errors.New(v.Description))
	}
	ce := invalidArgument("content.name", errs...)
	if suggestion, ok := itemname.Suggest(name); ok {
		if d, derr := connect.NewErrorDetail(&apiv1alpha1.NameSuggestion{Name: suggestion}); derr == nil {
			ce.AddDetail(d)
		}
	}
	return "", ce
}
