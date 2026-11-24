package auth

import (
	"encoding/json"
	"maps"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type Requester struct {
	Actor  core.Actor
	Origin map[string]string
}

func (r Requester) auditDetail(detail map[string]any) json.RawMessage {
	if len(r.Origin) > 0 {
		detail["origin"] = maps.Clone(r.Origin)
	}
	return mustJSON(detail)
}
