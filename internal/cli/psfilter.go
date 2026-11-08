package cli

import (
	"path"
	"strings"

	"github.com/ytjohn/fastener/internal/session"
)

const psFilterKeys = "label=K[=V], state=S, backend=B, name=GLOB"

// psFilter is one parsed `ps --filter KEY=VALUE`.
type psFilter struct {
	key, k, val string
	hasVal      bool // label only: label=K=V (true) vs label=K (exists)
}

var psStates = []string{session.StateStarting, session.StateRunning, session.StateExited, session.StateLost, session.StateRestoreFailed}

// parsePsFilters validates every --filter item; any problem is a usage error.
func parsePsFilters(items []string) ([]psFilter, error) {
	var out []psFilter
	for _, it := range items {
		key, val, ok := strings.Cut(it, "=")
		if !ok || val == "" {
			return nil, usagef("--filter %q: want KEY=VALUE; valid keys: %s", it, psFilterKeys)
		}
		f := psFilter{key: key, val: val}
		switch key {
		case "label":
			f.k, f.val, f.hasVal = strings.Cut(val, "=")
			if f.k == "" {
				return nil, usagef("--filter %q: label needs a key (label=K or label=K=V)", it)
			}
		case "state":
			valid := false
			for _, s := range psStates {
				valid = valid || s == val
			}
			if !valid {
				return nil, usagef("--filter %q: unknown state; want one of %s", it, strings.Join(psStates, ", "))
			}
		case "backend":
		case "name":
			if _, err := path.Match(val, ""); err != nil {
				return nil, usagef("--filter %q: bad glob: %v", it, err)
			}
		default:
			return nil, usagef("--filter %q: unknown key %q; valid keys: %s", it, key, psFilterKeys)
		}
		out = append(out, f)
	}
	return out, nil
}

// matches reports whether the row satisfies this filter.
func (f psFilter) matches(r psRow) bool {
	switch f.key {
	case "label":
		v, ok := r.Labels[f.k]
		return ok && (!f.hasVal || v == f.val)
	case "state":
		return r.State == f.val
	case "backend":
		return r.Backend == f.val
	case "name":
		ok, _ := path.Match(f.val, r.Name)
		return ok
	}
	return false
}

func matchAll(fs []psFilter, r psRow) bool {
	for _, f := range fs {
		if !f.matches(r) {
			return false
		}
	}
	return true
}
