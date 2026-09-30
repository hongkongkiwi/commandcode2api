// Downstream credential resolution: pass-through mode (client user_* key
// goes upstream unchanged) or pool mode (gateway sk-* key draws from the
// CC key pool).
package pool

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/hongkongkiwi/commandcode2api/internal/store"
)

// Mode of a resolved request credential.
type Mode int

const (
	ModeNone Mode = iota
	ModePassthrough
	ModePool
)

// Resolved credential for one request.
type Resolved struct {
	Mode Mode
	// PassthroughKey is the client's user_* key (ModePassthrough).
	PassthroughKey string
	// Gateway is the matched gateway key row (ModePool).
	Gateway *store.GatewayKey
}

var userKeyRe = regexp.MustCompile(`user_[a-zA-Z0-9_-]+`)

// Resolver authenticates downstream requests.
type Resolver struct {
	Store *store.Store
	Pool  *Pool
	// PassEnabled keeps pass-through available when the pool is on (dual
	// mode). When false, only gateway keys authenticate.
	PassEnabled bool
}

// Resolve inspects Authorization / x-api-key.
func (r *Resolver) Resolve(header http.Header) *Resolved {
	var raw string
	if auth := header.Get("Authorization"); len(auth) > 7 && auth[:7] == "Bearer " {
		raw = strings.TrimSpace(auth[7:])
	} else if x := header.Get("X-Api-Key"); x != "" {
		raw = strings.TrimSpace(x)
	}
	if raw == "" {
		return &Resolved{Mode: ModeNone}
	}

	if strings.HasPrefix(raw, "sk-") {
		gw, err := r.Store.FindGatewayKey(raw)
		if err != nil || gw == nil || !gw.Enabled {
			return &Resolved{Mode: ModeNone}
		}
		return &Resolved{Mode: ModePool, Gateway: gw}
	}

	if r.PassEnabled {
		if m := userKeyRe.FindString(raw); m != "" {
			return &Resolved{Mode: ModePassthrough, PassthroughKey: m}
		}
	}
	return &Resolved{Mode: ModeNone}
}
