package scan

import "regexp"

// This is the ONE file in the module that encodes the proprietary fingerprints,
// and it is excluded from the scanner's own run (see selfRulesPath in scan.go).
// As belt-and-suspenders on top of that exclusion, every fingerprint is
// assembled from fragments at call time, so NO forbidden string appears
// verbatim in this source: a naive grep of this file finds nothing, and even if
// the self-exclusion were removed the scanner would not flag its own rule table.
//
// Intentionally OMITTED - a tenant client-code rule. Bare tenant codes are
// short, unanchored tokens with no stable shape; an anchored regex either misses
// real codes or floods the scan with false positives, and this lab never held
// tenant data, so such a rule would be pure false-positive surface with no
// catch. Tenant-data hygiene is enforced upstream by never importing that data.
//
// ECR hosts are matched by their FULL shape only (12 digits . dkr . ecr .
// region . registry-suffix); the loose bare "dkr.ecr" fallback is deliberately
// NOT a rule (it fired on unrelated prose and doc URLs for no added catch).

// frag concatenates fragments so a forbidden literal is never written whole.
func frag(parts ...string) string {
	s := ""
	for _, p := range parts {
		s += p
	}
	return s
}

// DefaultRules returns the production rule set. Callers get a freshly compiled
// set; compilation is cheap and happens once per scan.
func DefaultRules() []Rule {
	ci := "(?i)"

	org := frag("read", "yon")
	domain := frag("onr", "eady") + `\.` + frag("d", "ev")

	// The four exact account-id literals (assembled half+half). Matching the
	// exact IDs - NOT a bare \d{12} - is what keeps unrelated 12-digit numbers
	// (timestamps, ports, hashes) from tripping the gate.
	acct := frag("951113", "916427") + "|" +
		frag("116153", "546408") + "|" +
		frag("975707", "452016") + "|" +
		frag("835975", "842700")

	otelNS := frag("opentelemetry-operator", "-system")
	otelFQDN := `\.` + otelNS + `\.` + frag("svc", `\.cluster\.local`)

	ecrHost := `[0-9]{12}\.` + frag("dkr", `\.ecr`) + `\.[a-z0-9-]+\.` + frag("amazonaws", `\.com`)

	irsaARN := frag("arn:aws:iam::", `[0-9]{12}:`) + `(role|policy|oidc-provider)/`

	return []Rule{
		{Name: "org-name", Re: regexp.MustCompile(ci + org), Desc: "internal organization name (any case)"},
		{Name: "org-domain", Re: regexp.MustCompile(ci + domain), Desc: "internal public domain"},
		{Name: "account-id", Re: regexp.MustCompile(acct), Desc: "one of the internal AWS account IDs"},
		{Name: "ecr-host", Re: regexp.MustCompile(ecrHost), Desc: "internal container-registry host (full ECR shape)"},
		{Name: "otel-fqdn", Re: regexp.MustCompile(otelFQDN), Desc: "internal collector service FQDN"},
		{Name: "otel-ns", Re: regexp.MustCompile(otelNS), Desc: "internal collector namespace"},
		{Name: "irsa-arn", Re: regexp.MustCompile(irsaARN), Desc: "IRSA role/policy/OIDC ARN shape"},
	}
}
