package resourcefiltering

import "slices"

// Matcher determines whether a given string passes a filter rule.
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

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *AllowDeny) DeepCopyInto(out *AllowDeny) {
	*out = *in
	if in.Allow != nil {
		out.Allow = &Allow{}
		if in.Allow.Literal != nil {
			out.Allow.Literal = make([]string, len(in.Allow.Literal))
			copy(out.Allow.Literal, in.Allow.Literal)
		}
	}
	if in.Deny != nil {
		out.Deny = &Deny{}
		if in.Deny.Literal != nil {
			out.Deny.Literal = make([]string, len(in.Deny.Literal))
			copy(out.Deny.Literal, in.Deny.Literal)
		}
	}
}

// DeepCopy returns a deep copy of the AllowDeny.
func (in *AllowDeny) DeepCopy() *AllowDeny {
	if in == nil {
		return nil
	}
	out := new(AllowDeny)
	in.DeepCopyInto(out)
	return out
}
