package expr

import "github.com/raintank/metrictank/api/models"

type FuncAlias struct {
	alias string
}

func NewAlias() Func {
	return FuncAlias{}
}

func (s FuncAlias) Signature() ([]argType, []argType) {
	return []argType{seriesList, str}, []argType{seriesList}
}

func (s FuncAlias) Init(args []*expr) error {
	s.alias = args[1].valStr
	return nil
}

func (s FuncAlias) Depends(from, to uint32) (uint32, uint32) {
	return from, to
}

func (s FuncAlias) Exec(cache map[Req][]models.Series, in ...interface{}) ([]interface{}, error) {
	series, ok := in[0].([]models.Series)
	if !ok {
		return nil, ErrArgumentBadType
	}
	var out []interface{}
	for _, serie := range series {
		serie.Target = s.alias
		out = append(out, s)
	}
	return out, nil
}