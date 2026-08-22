package tools

import (
	"encoding/json"
	"io"
)

// jsonDecoder 是开启 DisallowUnknownFields 的 json.Decoder 薄封装。catalog 是发布物，
// 出现未知字段说明 descriptor 与 Go model 已经漂移，必须直接失败而不是静默丢字段。
type jsonDecoder struct {
	*json.Decoder
}

func newStrictDecoder(reader io.Reader) *jsonDecoder {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	return &jsonDecoder{Decoder: decoder}
}
