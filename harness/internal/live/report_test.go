package live

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/model"
	"github.com/MateSousa/istio-ambient-upgrade-lab/harness/internal/report"
)

var rt0 = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

func writeJSONFile(t *testing.T, dir, name string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func writeRawFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func validResult(verdict, reason string) model.Result {
	return model.Result{
		SchemaVersion:        model.SchemaVersion,
		Verdict:              verdict,
		Reason:               reason,
		Trigger:              model.TriggerInfo{Kind: "git-bump", ACSatisfying: true},
		RecoveryBoundSeconds: 30,
		Corroboration:        model.Corroboration{Note: "none"},
		Anomalies:            []model.Anomaly{},
		PerNodeAttribution:   []model.PerNodeAttribution{},
	}
}

func runReport(t *testing.T, in, clients string) (int, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "report.md")
	return RunReport(ReportConfig{InPath: in, ClientsPath: clients, OutPath: out, GeneratedAt: rt0})
}

// ------------------------------------------------------------------ R15 ------
// attributeClientReset is the pure attribution core: a reset inside the window
// (widened by eps) is upgrade-attributable; churn outside is not.
func TestR15_AttributeClientReset(t *testing.T) {
	eps := 2 * time.Second
	drain := rt0
	w := model.RollWindow{Node: "n1", DrainingAt: drain, GraceExpiresAt: drain.Add(120 * time.Second)}
	cases := []struct {
		name string
		ts   time.Time
		want bool
	}{
		{"mid-window", drain.Add(60 * time.Second), true},
		{"at-drain", drain, true},
		{"at-grace-expiry", drain.Add(120 * time.Second), true},
		{"within-eps-before", drain.Add(-1 * time.Second), true},
		{"within-eps-after", drain.Add(121 * time.Second), true},
		{"churn-well-before", drain.Add(-30 * time.Second), false},
		{"churn-well-after", drain.Add(200 * time.Second), false},
		{"just-outside-eps-before", drain.Add(-3 * time.Second), false},
		{"just-outside-eps-after", drain.Add(123 * time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attributeClientReset(tc.ts, w, eps); got != tc.want {
				t.Fatalf("attributeClientReset(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// ------------------------------------------------------------------ R16 ------
func TestR16_ResultSchemaMismatch_Reject(t *testing.T) {
	dir := t.TempDir()
	bad := validResult(model.VerdictPass, "")
	bad.SchemaVersion = "harness/v0"
	in := writeJSONFile(t, dir, "results.json", bad)
	code, err := runReport(t, in, "")
	if code == 0 || err == nil {
		t.Fatalf("expected non-zero + error for bad result schema, got code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "harness/v0") || !strings.Contains(err.Error(), model.SchemaVersion) {
		t.Fatalf("error must name BOTH found and expected version: %v", err)
	}
}

// ------------------------------------------------------------------ R17 ------
func TestR17_ClientsSchemaMismatch_Reject(t *testing.T) {
	dir := t.TempDir()
	in := writeJSONFile(t, dir, "results.json", validResult(model.VerdictPass, ""))
	badClients := report.PerClientObservations{SchemaVersion: "clients/v0", Clients: []report.ClientObservation{{Client: "app-a"}}}
	cp := writeJSONFile(t, dir, "clients.json", badClients)
	code, err := runReport(t, in, cp)
	if code == 0 || err == nil {
		t.Fatalf("expected non-zero + error for bad clients schema, got code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "clients/v0") || !strings.Contains(err.Error(), report.ClientsSchemaVersion) {
		t.Fatalf("error must name BOTH found and expected clients version: %v", err)
	}
}

// ------------------------------------------------------------------ R18 ------
// Exit-code matrix: a FAIL or ERROR Result still renders (code 0); only IO,
// decode, or schema errors are non-zero.
func TestR18_ExitCodeMatrix(t *testing.T) {
	dir := t.TempDir()
	fail := writeJSONFile(t, dir, "fail.json", validResult(model.VerdictFail, model.ReasonNewConnFailure))
	erru := writeJSONFile(t, dir, "error.json", validResult(model.VerdictError, model.ReasonNoRolloutObserved))
	malformed := writeRawFile(t, dir, "malformed.json", "{ this is not json")
	badSchema := validResult(model.VerdictPass, "")
	badSchema.SchemaVersion = "harness/v99"
	badSchemaPath := writeJSONFile(t, dir, "badschema.json", badSchema)

	cases := []struct {
		name     string
		in       string
		wantZero bool
	}{
		{"FAIL renders (0)", fail, true},
		{"ERROR renders (0)", erru, true},
		{"missing file (nonzero)", filepath.Join(dir, "does-not-exist.json"), false},
		{"malformed (nonzero)", malformed, false},
		{"bad schema (nonzero)", badSchemaPath, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := runReport(t, tc.in, "")
			if tc.wantZero {
				if code != 0 || err != nil {
					t.Fatalf("want code 0/nil, got code=%d err=%v", code, err)
				}
			} else {
				if code == 0 || err == nil {
					t.Fatalf("want non-zero + error, got code=%d err=%v", code, err)
				}
			}
		})
	}
}
