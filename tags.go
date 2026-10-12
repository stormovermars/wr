package main

import (
	"strconv"
	"strings"
)

// Tenant tags and the archived flag come from `mw version -j` (mw >= 0.2.1), no extra call.
// An archived tenant is never dispatched and is left out of status and ui, which say how
// many they hid; --tag T (repeatable, AND) narrows status and ui to tenants with every tag.

func (m *MW) meta(slug string) MWTenant {
	t, _ := m.metaErr(slug)
	return t
}

// metaErr is meta plus the read error. Dispatch paths fail closed on it: a tenant whose
// metadata cannot be read is not dispatched.
func (m *MW) metaErr(slug string) (MWTenant, error) {
	ts, err := m.tenants() // version -j tenants, else `mw tenant list -j -v` (both carry tags/archived)
	if err != nil {
		return MWTenant{Tenant: slug}, err
	}
	for _, t := range ts {
		if t.Tenant == slug {
			return t, nil
		}
	}
	return MWTenant{Tenant: slug}, nil
}

// shown filters slugs for a view: archived ones are counted and dropped, then the tag filter.
func (d *Dispatcher) shown(slugs []string) (out []string, archived int) {
	for _, s := range slugs {
		mt := d.MW.meta(s)
		if mt.Archived {
			archived++
		} else if hasTags(mt.Tags, d.Tags) {
			out = append(out, s)
		}
	}
	return out, archived
}

func hasTags(have []string, want string) bool {
	for _, w := range strings.Split(want, ",") {
		if w = strings.ToLower(strings.TrimSpace(w)); w == "" {
			continue
		}
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}

// tagText is " #a #b" for terminal views.
func tagText(tags []string) string {
	s := ""
	for _, t := range tags {
		s += " #" + t
	}
	return s
}

func archivedNote(n int) string {
	if n == 0 {
		return ""
	}
	if n == 1 {
		return "archived: 1 tenant hidden"
	}
	return "archived: " + strconv.Itoa(n) + " tenants hidden"
}
