// +kubebuilder:object:generate=true
package resourcefiltering

import "slices"

// Matcher determines whether a given string passes a filter rule.
// +kubebuilder:object:generate=false
type Matcher interface {
	Pass(s string) bool
}

// Allow permits only the strings listed in Literal.
type Allow struct {
	Literal []string `json:"literal,omitempty"`
}

// Pass returns true if s is present in the Literal slice.
func (a *Allow) Pass(s string) bool {
	return slices.Contains(a.Literal, s)
}

// Deny rejects the strings listed in Literal.
type Deny struct {
	Literal []string `json:"literal,omitempty"`
}

// Pass returns true if s is not present in the Literal slice.
func (d *Deny) Pass(s string) bool {
	return !slices.Contains(d.Literal, s)
}

// AllowDeny defines allow and deny lists for resource filtering.
//
// Evaluation semantics (via Pass):
//   - nil AllowDeny → false (no configuration means nothing passes)
//   - Allow is non-nil → only Allow is evaluated (Deny is ignored)
//   - Allow is nil, Deny is non-nil → only Deny is evaluated
//   - Both nil → true (everything passes)
type AllowDeny struct {
	Allow *Allow `json:"allow,omitempty"`
	Deny  *Deny  `json:"deny,omitempty"`
}

// Pass returns whether s is permitted by the AllowDeny configuration.
// If Allow is set, only Allow is evaluated and Deny is ignored.
// If Allow is nil and Deny is set, only Deny is evaluated.
// If both are nil, all values pass.
func (ad *AllowDeny) Pass(s string) bool {
	if ad.Allow != nil {
		return ad.Allow.Pass(s)
	}
	if ad.Deny != nil {
		return ad.Deny.Pass(s)
	}
	return true
}

// IsAllowed is a nil-safe convenience wrapper around AllowDeny.Pass.
//
// Semantics:
//   - nil  → false (no configuration means the name is not allowed by this filter)
//   - non-nil → delegates to ad.Pass(name)
func IsAllowed(ad *AllowDeny, name string) bool {
	if ad == nil {
		return false
	}
	return ad.Pass(name)
}
